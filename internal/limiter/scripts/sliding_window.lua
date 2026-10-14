-- Sliding window counter, one atomic step: read both windows, estimate, decide, write.
-- KEYS[1] current window counter   KEYS[2] previous window counter
-- ARGV[1] limit  ARGV[2] period in ms  ARGV[3] cost  ARGV[4] now in ms
-- ARGV[5] start of the current window in ms (Go computes it, so Go and Lua
--         can never disagree about which window this is)
-- Returns four integers: allowed (1/0), remaining, retry_after_ms, reset_after_ms.
local limit = tonumber(ARGV[1])
local period = tonumber(ARGV[2])
local cost = tonumber(ARGV[3])
local now = tonumber(ARGV[4])
local window_start = tonumber(ARGV[5])

-- A missing key arrives as false, not nil; a missing window counts as zero.
local curr = tonumber(redis.call('GET', KEYS[1])) or 0
local prev = tonumber(redis.call('GET', KEYS[2])) or 0

local elapsed = now - window_start
if elapsed < 0 then
  elapsed = 0
elseif elapsed > period then
  elapsed = period
end

-- The previous window still counts, weighted by how much of it is still
-- inside the sliding window.
local estimate = prev * (period - elapsed) / period + curr

local allowed = 0
local remaining
if estimate + cost <= limit then
  allowed = 1
  curr = redis.call('INCRBY', KEYS[1], cost)
  -- 2 x period, not 1: this key becomes "previous" in the next window and must
  -- live that long. With 1 x, prev would always read 0 and the algorithm would
  -- silently turn into a fixed window.
  redis.call('PEXPIRE', KEYS[1], 2 * period)
  remaining = limit - estimate - cost
else
  -- A denied request writes nothing.
  remaining = limit - estimate
end
if remaining < 0 then
  remaining = 0
end

local retry_after = 0
if allowed == 0 then
  if curr + cost <= limit then
    -- Case A: the current window has room; we only wait for the previous
    -- window's weight to shrink. prev > 0 here, otherwise we would be allowed.
    retry_after = (period - elapsed) - period * (limit - cost - curr) / prev
  else
    -- Case B: the current window itself is full; wait for the next window, where
    -- it becomes "previous" and its weight shrinks. curr > 0 here, because
    -- cost <= limit was validated.
    retry_after = (period - elapsed) + math.max(0, period - period * (limit - cost) / curr)
  end
  retry_after = math.max(0, math.ceil(retry_after))
end

-- Time until nothing from this window or the one before still counts.
local reset_after = period - elapsed
if curr > 0 then
  reset_after = reset_after + period
end

-- Integers only: Lua numbers in a reply are truncated.
return { allowed, math.floor(remaining), retry_after, math.ceil(reset_after) }
