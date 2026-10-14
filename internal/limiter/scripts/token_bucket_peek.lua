#!lua flags=no-writes
-- Same math as token_bucket.lua with every write removed. The shebang makes
-- Redis itself reject any write command, so Peek cannot change state even by
-- mistake. It must return exactly what Allow would return right now.
-- KEYS[1] bucket key   ARGV: capacity, rate per ms, cost, now ms
local capacity = tonumber(ARGV[1])
local rate = tonumber(ARGV[2])
local cost = tonumber(ARGV[3])
local now = tonumber(ARGV[4])

local data = redis.call('HMGET', KEYS[1], 'tokens', 'ts')
local tokens, ts
if data[1] == false or data[2] == false then
  tokens = capacity
  ts = now
else
  tokens = tonumber(data[1])
  ts = tonumber(data[2])
end

local elapsed = now - ts
if elapsed < 0 then
  elapsed = 0
end
tokens = math.min(capacity, tokens + elapsed * rate)

local allowed = 0
if tokens >= cost then
  allowed = 1
  tokens = tokens - cost -- what Allow would leave behind; not written
end

local retry_after = 0
if allowed == 0 then
  retry_after = math.ceil((cost - tokens) / rate)
end
local reset_after = math.ceil((capacity - tokens) / rate)

return { allowed, math.floor(tokens), retry_after, reset_after }
