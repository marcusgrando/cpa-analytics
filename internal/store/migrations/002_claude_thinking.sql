-- Plugin 0.1.3 and earlier stored Claude calls with thinking as inconsistent:
-- CPA's Claude executor counts thinking inside output_tokens, and the port
-- treated it as separate, so the total never matched. The raw counters were
-- kept, so the buckets can be rebuilt. The provider test mirrors
-- accounting.semanticsFor for Anthropic, and the counter test mirrors
-- accounting.claudeThinkingInsideOutput.
UPDATE requests
SET uncached_input       = input_tokens,
    cache_read           = cache_read_tokens,
    cache_write          = cache_creation_tokens,
    output_non_reasoning = output_tokens - reasoning_tokens,
    output_reasoning     = reasoning_tokens,
    unclassified         = 0,
    accounting_quality   = 'complete'
WHERE accounting_quality = 'inconsistent'
  AND lower(trim(executor_type)) <> 'openaicompatexecutor'
  AND lower(trim(provider)) <> 'openai-compatibility'
  AND lower(trim(provider)) NOT LIKE 'openai-compatible-%'
  AND (lower(provider || ' ' || executor_type) LIKE '%claude%' OR lower(provider || ' ' || executor_type) LIKE '%anthropic%')
  AND input_tokens >= 0 AND cache_read_tokens >= 0 AND cache_creation_tokens >= 0
  AND reasoning_tokens > 0 AND reasoning_tokens <= output_tokens
  AND total_tokens > 0
  AND total_tokens = input_tokens + cache_read_tokens + cache_creation_tokens + output_tokens;
