#!lua flags=no-writes
-- Same math as sliding_window.lua with every write removed. It must return
-- exactly what Allow would return right now.
-- KEYS[1] current  KEYS[2] previous
-- ARGV: limit, period ms, cost, now ms, window start ms
local limit = tonumber(ARGV[1])
local period = tonumber(ARGV[2])
local cost = tonumber(ARGV[3])
local now = tonumber(ARGV[4])
local window_start = tonumber(ARGV[5])

local curr = tonumber(redis.call('GET', KEYS[1])) or 0
local prev = tonumber(redis.call('GET', KEYS[2])) or 0

local elapsed = now - window_start
if elapsed < 0 then
  elapsed = 0
elseif elapsed > period then
  elapsed = period
end

local estimate = prev * (period - elapsed) / period + curr

local allowed = 0
local remaining
if estimate + cost <= limit then
  allowed = 1
  curr = curr + cost -- what Allow's INCRBY would leave behind; not written
  remaining = limit - estimate - cost
else
  remaining = limit - estimate
end
if remaining < 0 then
  remaining = 0
end

local retry_after = 0
if allowed == 0 then
  if curr + cost <= limit then
    retry_after = (period - elapsed) - period * (limit - cost - curr) / prev
  else
    retry_after = (period - elapsed) + math.max(0, period - period * (limit - cost) / curr)
  end
  retry_after = math.max(0, math.ceil(retry_after))
end

local reset_after = period - elapsed
if curr > 0 then
  reset_after = reset_after + period
end

return { allowed, math.floor(remaining), retry_after, math.ceil(reset_after) }
