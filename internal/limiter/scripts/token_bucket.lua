-- Token bucket, one atomic step: read state, refill, decide, write.
-- KEYS[1] bucket key (HASH: tokens, ts)
-- ARGV[1] capacity  ARGV[2] rate in tokens per ms  ARGV[3] cost  ARGV[4] now in ms
-- Returns four integers: allowed (1/0), remaining, retry_after_ms, reset_after_ms.
local capacity = tonumber(ARGV[1])
local rate = tonumber(ARGV[2])
local cost = tonumber(ARGV[3])
local now = tonumber(ARGV[4])

-- A missing HASH field arrives as false, not nil.
local data = redis.call('HMGET', KEYS[1], 'tokens', 'ts')
local tokens, ts
if data[1] == false or data[2] == false then
  -- New (or expired) bucket starts full.
  tokens = capacity
  ts = now
else
  tokens = tonumber(data[1])
  ts = tonumber(data[2])
end

-- Clocks of different instances can disagree; never refill backwards.
local elapsed = now - ts
if elapsed < 0 then
  elapsed = 0
end
tokens = math.min(capacity, tokens + elapsed * rate)

local allowed = 0
if tokens >= cost then
  allowed = 1
  tokens = tokens - cost
  -- The key expires exactly when the bucket would be full again, so TTL
  -- never changes behaviour; it only frees memory.
  local ttl = math.ceil((capacity - tokens) / rate)
  if ttl < 1 then
    ttl = 1
  end
  -- Full precision: Lua's default number-to-string drops digits and tokens
  -- would drift over time.
  redis.call('HSET', KEYS[1], 'tokens', string.format('%.17g', tokens), 'ts', now)
  redis.call('PEXPIRE', KEYS[1], ttl)
end
-- A denied request writes nothing: under attack most calls are denials and
-- writing on each one would double the load on Redis.

local retry_after = 0
if allowed == 0 then
  retry_after = math.ceil((cost - tokens) / rate)
end
local reset_after = math.ceil((capacity - tokens) / rate)

-- Integers only: Lua numbers in a reply are truncated.
return { allowed, math.floor(tokens), retry_after, reset_after }
