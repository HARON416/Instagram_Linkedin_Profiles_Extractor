# Instagram / LinkedIn Profile Matcher

Discovers profile candidates through TinyFish, captures public profile content in
Chrome, and uses Jev through OpenRouter to judge whether Instagram and LinkedIn
profiles belong to the requested person.

## Setup

Use Go 1.25.6 or later, Chrome, and a `names.xlsx` input workbook. Configure the
existing TinyFish search credentials and an OpenRouter account with credits:

Bash (Linux/macOS):

```bash
export TINYFISH_API_KEY='your-tinyfish-key'
export OPENROUTER_API_KEY='your-openrouter-key'
go run .
```

Windows Command Prompt:

```bat
set "TINYFISH_API_KEY=your-tinyfish-key"
set "OPENROUTER_API_KEY=your-openrouter-key"
go run .
```

Browser capture always reuses one window, processing profiles sequentially.
Instagram uses GraphQL request replay: Rod intercepts the current profile request,
then Go sends a copy with its payload, headers, and active session cookies. Chrome
continues its original request. The former passive CDP response reader is removed;
there is no fallback to it. A browser login is still required when prompted.
LinkedIn continues to use rendered page content.

Instagram visits are spaced at least five seconds apart, with at most one GraphQL
replay per candidate. HTTP 429 is recorded as a rate-limit error immediately when
received by the capture waiter. The affected candidate is skipped; subsequent
Instagram visits share a cooldown starting at one minute, doubling on repeated
429s up to a five-minute fallback. A longer server `Retry-After` is honored.
Successful captures reset the escalation. The browser leaves failed profile
pages to stop their background traffic. These limits are local defaults, not
Instagram-published quotas; they reduce pressure but cannot guarantee access.
Cooldown state lasts for the current run, so restarting the process resets it.
A new run can retry failed candidates after the restriction clears.
`ROD_WINDOWS` and the legacy `ROD_WORKERS` setting are no longer used.
Complete browser sign-in when prompted. The current `maximumTestNames` setting in
`main.go` processes only the first name.

Optional Jev settings:

```bash
export JEV_MODEL='typesafe/jev-1.13'
export JEV_MATCH_THRESHOLD='0.90'
```

The threshold must be greater than 0 and at most 1. The default 0.90 is provisional,
not calibrated on verified profile pairs. Tune it against manually verified
matches and nonmatches before relying on automated decisions. Keys belong in your
local environment, not in source control.

TinyFish discovery retries transient HTTP 408, 429, 500, 502, 503, and 504 errors,
network timeouts, and interrupted response reads up to four total attempts.
Retries wait 1, 2, and 4 seconds unless the server supplies `Retry-After`, and each
attempt counts against the local search quota. Authentication and other permanent
errors fail immediately. If retries are exhausted, partial discovery results are
saved and the run stops before enrichment.

## Matching and output

The application calls `POST https://openrouter.ai/api/alpha/decisions`, asking one
Noul (yes/no) question per captured Instagram candidate. All questions share the
LinkedIn baseline and profile evidence. The question requires the baseline to
support the requested name and sufficient corroboration beyond a shared name.
Jev does not generate explanations.

`profile_candidates.json` stores each evaluated candidate as:

```json
{"match_analysis": {"match": true, "score": 0.9432}}
```

`score` is the raw model probability (0–1) that the supplied evidence supports a
match, not a measured identity accuracy or the previous weighted rubric score.
`match` is true when `score >= JEV_MATCH_THRESHOLD`. All candidates are judged
independently; more than one can pass the threshold.

The person-level result and `profile_matches.xlsx` show the highest-scoring
candidate, its score, and Yes/No. The URL identifies the evaluated candidate even
when the answer is No. Ties use the first candidate. Excel displays probabilities
as percentages. Errors and skipped comparisons have blank match/score cells and
an explicit status in Run Details; they are not negative matches.

The JSON records the configured model and threshold, plus the returned model
snapshot per person when available. Gemini credentials and the Google GenAI SDK
are no longer used. Old component scores, explanations, review flags, and
Gemini-specific metadata are removed when results are regenerated. Existing
output files are not migrated automatically; rerun the pipeline to regenerate
them. Consumers must account for `best_match_score` changing from 0–100 points to
a 0–1 probability and the new nullable `match` field.

Jev currently supports a 32K-token context. The existing LinkedIn text limit is
40,000 characters, not a token guarantee; oversized requests surface as API
errors. Search behavior is unchanged.

API reference and request examples:
[Jev tutorial](https://openrouter.ai/docs/guides/community/jev-tutorial).

## Tests

```bash
go test ./...
```

Tests use local HTTP servers, with no paid model calls or live browser sessions.
