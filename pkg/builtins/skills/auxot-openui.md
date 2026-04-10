# OpenUI Lang — App & Widget Rendering

Output an `openui` code fence to build full-page apps or inline widgets with charts, tables, metrics, and live data.

**Read this skill first whenever building an OpenUI app or widget.**

## How to deliver your output

> **CRITICAL**: Write the `openui` code fence **directly in your text response**. Do NOT use `write`, `bash`, or any other tool to create a file. The platform intercepts the fence from your response and renders it as the live application. Writing a file does nothing.

````
```openui
root = ...
```
````

Your entire response must be ONLY the code fence — no explanation, no prose, no other text before or after it.

## CRITICAL: you MUST assign `root`

**The identifier must be literally `root`.** One assignment: `root = <expression>`.

The renderer uses that node as the full page. If you omit `root` and only write `header = ...`, `sectionA = ...`, `sectionB = ...`, the parser falls back to the **last** top-level non-datasource variable — often **only the last card or section renders**, and the rest never appear. That is why a missing `root` looks like “only the buttons at the bottom showed up.”

**Correct pattern**

1. Define pieces: `header = OHeader("Title", "Subtitle")`, `metrics = Stack([...], "horizontal", 6)`, `main = Card([...])`, etc.
2. Compose once: `root = Stack([header, metrics, main], "col", 8)` (gap and direction as needed).

`root` may appear **anywhere** in the file (top or bottom); what matters is that it exists and references **every** section the user should see.

## CRITICAL: syntax is not JSX or React

OpenUI Lang uses **commas, lists, and positional arguments** inside `Component(...)`.

| Wrong (JSX / named props) | Right (OpenUI) |
|---------------------------|----------------|
| `OHeader(title = "A", subtitle = "B")` | `OHeader("A", "B")` |
| `StatCard(label = "X", value = "1")` | `StatCard("X", "1")` |
| `Card(children = Stack([...]))` | `Card([Stack([...], "col", 6)])` |
| `Stack(direction = "horizontal", children = [...])` | `Stack([...], "horizontal", 6)` |

Optional kwargs, when supported, use `name: value` or `name = value` **only** in the small DSL style shown in examples — not React prop syntax.

## Assignment rules

- Every line: `identifier = Expression`
- Every component call **must** be assigned to a variable — no bare calls
- **`root = ...` is mandatory** for apps and widgets — that identifier is the page/widget root; see CRITICAL section above

## Components

| Name | Arguments | Notes |
|------|-----------|-------|
| `OHeader(title, subtitle?)` | strings | Page title — **required first in apps** |
| `Card(children)` | list | Container with subtle border |
| `Stack(children, direction?, gap?)` | list, "horizontal"\|"row"\|"col", number | Flex layout; use `"horizontal"` or `"row"` for side-by-side |
| `StatCard(label, value, trend?)` | strings, "up"\|"down"\|"flat" | KPI metric. **Omit `trend` unless you can compute a real direction from the data.** Never hardcode `"up"` — it misleads users. |
| `Table(rows, columns)` | data ref, string list | Tabular data; columns are field-name strings. Use `"field:linkField"` to make a column clickable — see below. |
| `BarChart(data, xField, yField)` | data ref, strings | Bar chart |
| `LineChart(data, xField, yField)` | data ref, strings | Line chart |
| `Text(content)` | markdown string | Prose, headings, labels |
| `Badge(text, variant?)` | string, "primary"\|"success"\|"warning"\|"error" | Status pill |
| `Progress(value, variant?)` | 0–100, variant | Progress bar |
| `Alert(message, type?)` | string, "info"\|"success"\|"warning"\|"error" | Alert box |
| `Button(label, variant?)` | string, "primary"\|"secondary"\|"ghost" | Action button |
| `Link(text, href)` | strings | Inline hyperlink; opens in new tab |

## Clickable table columns

Append `:linkField` to a column name to make cells in that column render as links. The `linkField` is the name of the field in each row that holds the URL.

```
# Fetch stories with title, url, and points
stories = bash({"command": "curl -s '...' | jq '[.hits[] | {title, url, points}]'"})

# "title:url" → the title text is a link; href comes from each row's "url" field
table = Table(stories, ["title:url", "points"])
```

Rules:
- The part before `:` is the display field (shown as column header and cell text)
- The part after `:` is the href field — it must be a URL string in each row
- Rows where the href field is empty or missing fall back to plain text
- Use this whenever the data includes a `url` or `link` field alongside human-readable names

## Standalone links

Use `Link(text, href)` for inline hyperlinks outside a table:

```
header_link = Link("Open Hacker News", "https://news.ycombinator.com")
card = Card([Text("## Resources"), header_link])
```

## Data sources

Data sources are resolved at render time. Assign them to identifiers and reference those identifiers in component arguments.

### Tool source (preferred for live data)

Call any available tool by its exact name. Pass arguments as a JSON object — **the same shapes you would use in an actual tool call** (same keys the worker expects).

```
stories = brave_search({"query": "hacker news top stories today"})
repos   = github({"command": "list_repositories"})
```

**`bash` is no exception:** use `bash({"command": "shell command here"})` only. A bare string like `bash("curl ...")` is wrong — it does not match the tool schema and will fail at refresh time.

**`bash` for HTTP/API data — output MUST be valid JSON:**

Pipe `curl` through `jq` or `python3` to filter and reshape the response into valid JSON. Raw text output (grep, awk, sed) will not render in components.

```
# jq — concise, best for simple field selection and array mapping
hn_front = bash({"command": "curl -s 'https://hn.algolia.com/api/v1/search?tags=front_page&hitsPerPage=25' | jq '[.hits // [] | .[] | {title, points, comments: .num_comments}]'"})

ai_news  = bash({"command": "curl -s 'https://hn.algolia.com/api/v1/search?tags=story&q=AI+OR+machine+learning&hitsPerPage=15' | jq '[.hits // [] | .[] | {title, points}]'"})

# python3 — better for complex reshaping, math, or environments without jq
disk = bash({"command": "df -h / | python3 -c \"import sys; p=sys.stdin.read().split(); print('{\\\"used\\\":\\\"'+p[9]+'\\\",\\\"avail\\\":\\\"'+p[10]+'\\\",\\\"pct\\\":\\\"'+p[11]+'\\\"}')\" "})

hn_py = bash({"command": "curl -s 'https://hn.algolia.com/api/v1/search?tags=front_page&hitsPerPage=25' | python3 -c \"import json,sys; d=json.load(sys.stdin); print(json.dumps([{'title':h['title'],'points':h.get('points',0)} for h in d.get('hits',[])]))\""})
```

Rules:
- Output **must** be valid JSON — use `jq` or `python3`, never raw `grep`/`awk`/`sed`
- **URL-encode** query parameters with spaces — use `%20` or `+` (e.g. `q=AI+OR+machine+learning`, NOT `q=AI OR machine learning`). Unencoded spaces cause curl to silently fail.
- Prefer `jq` for simple field selection; use `python3` for complex transformations
- Shape as `[{...}, ...]` for tables, `{...}` for stat cards / scalars
- When jq fails with "Cannot iterate over null", the API may be returning an error; add `// empty` to jq expressions to handle nulls gracefully: `.hits // [] | .[] | ...`

### Computed values (client-side)

Use `compute()` to derive a JavaScript value from already-resolved datasources. Every datasource identifier is in scope.

```
hn_front    = bash({"command": "..."})   # resolves to [{title, points}, ...]
story_count = compute("Array.isArray(hn_front) ? hn_front.length : 0")
top_title   = compute("Array.isArray(hn_front) && hn_front[0] ? hn_front[0].title : ''")
```

`compute()` rules:
- The client runs `compute()` only after **every** bash/llm datasource has finished its first load (nothing still **pending**). Until then, derived metrics may show as loading. If a datasource errors, it is passed into scope as `{ error: "…" }` — guard with optional checks if needed.
- Single JavaScript expression only (no statements, no `return`)
- **No string concatenation in component arguments** — the lexer does not treat `+` as an operator (it is ignored). Write `StatCard("Label", x)` or put formatting inside `compute(" \"$\" + revenue ")`, never `StatCard("Label", "$" + x)`.
- **No method calls in component arguments** — expressions like `total_pageviews.toLocaleString()`, `n.toFixed(1)`, or `x.map(...)` are **not** run in the UI tree; they resolve to nothing and `StatCard` shows a **skeleton**. Only `ref`, `ref.field`, and literals work there. Put `.toLocaleString()`, `+ "K"`, etc. **inside** `compute("...")`, then pass the computed identifier: `pv_label = compute("(…).toLocaleString() + 'K'")` → `StatCard("Page Views", pv_label, "up")`.
- Use optional chaining and defaults to handle unresolved dependencies gracefully
- Use for counts, derived metrics, and simple transformations of resolved data
- Do NOT use for fetching — that is what `bash()` and `llm()` are for

`StatCard` trend — only set when you have real comparative data. Omit entirely otherwise. **Never hardcode `"up"` or `"down"` — it is meaningless decoration.**

```
# No baseline → omit trend
count      = compute("Array.isArray(stories) ? stories.length : 0")
count_card = StatCard("Stories", count)

# Has current vs previous → compute direction
trend    = compute("data.current > data.previous ? 'up' : data.current < data.previous ? 'down' : 'flat'")
kpi_card = StatCard("Weekly Active Users", data.current, trend)
```

- The tool name is the identifier before `(...)` — use the exact name the agent has available
- Arguments are a JSON object literal `{...}` (for `bash`, always include `"command": "..."`) or can be omitted for no-argument tools
- The result is a JSON value (array, object, or scalar); reference fields with dot notation

### LLM source (for reasoning, combining, or transforming)

```
summary = llm("Fetch the top 5 HN stories using brave_search and summarise them as [{title, url, summary}]")
disk    = llm("Get disk usage via bash. Return {used_gb, free_gb, percent_used}")
```

Use `llm()` when you need the LLM to:
- Make multiple tool calls and combine results
- Reason over raw data before returning structured JSON
- Transform or filter data from another source

The prompt must describe the **exact JSON shape** to return.

### Static data

```
labels = ["Mon", "Tue", "Wed", "Thu", "Fri"]
values = [12, 19, 8, 24, 17]
```

### Dot-notation field access

```
disk     = bash({"command": "df -h /"})
diskUsed = StatCard("Disk used", disk.percent_used)
```

## Constraint: one source per identifier

Never combine two datasource refs in the same expression. Use `llm()` to merge:

```
# WRONG
root = Table(a.concat(b), ["title"])

# RIGHT
combined = llm("Fetch from source A and source B, return [{title}]")
root = Table(combined, ["title"])
```

## Full app layout rules

- **`root = ...` must include the full page** — header, metrics, charts, tables, actions — not just the last section
- `OHeader` should be the first child inside the top-level `Stack` assigned to `root`
- Compose sections vertically with a top-level `Stack` in `"col"` direction
- Wrap each section in a `Card`
- Use a horizontal `Stack` of `StatCard`s for a metrics row near the top
- Aim for 2–4 distinct sections

## Example: HN dashboard (with tool data)

```openui
root = Stack([header, metrics_row, stories_section], "col", 8)

header = OHeader("Hacker News Dashboard", "Top stories and trending topics")

# Fetch live data — pipe curl through jq or python3 to produce valid JSON
hn_front = bash({"command": "curl -s 'https://hn.algolia.com/api/v1/search?tags=front_page&hitsPerPage=25' | jq '[.hits // [] | .[] | {title, url, points, comments: .num_comments}]'"})

# Derive metrics client-side with compute() — omit trend when there's no comparison data
story_count = compute("Array.isArray(hn_front) ? hn_front.length : 0")
top_points  = compute("Array.isArray(hn_front) && hn_front[0] ? hn_front[0].points : 0")

metrics_row = Stack([
    Card([StatCard("Front Page Stories", story_count)]),
    Card([StatCard("Top Story Points", top_points)])
], "horizontal", 6)

stories_section = Card([
    Text("## Top Stories"),
    Table(hn_front, ["title:url", "points", "comments"])
])
```

## Example: simpler widget

```openui
root = Card([title, table])
title = Text("## Open PRs")
prs = github({"command": "list_pull_requests", "repo": "owner/repo"})
table = Table(prs, ["number", "title", "user", "created_at"])
```

## Invalid syntax (never do this)

- **No program without `root = ...`** for apps and widgets — otherwise most of the UI will be missing
- **No JSX/React prop syntax** — no `name = value` or `children =` inside components (use positional args and `[child, ...]` lists)
- No inline JS assignments: `count = x.length` — use `compute("x ? x.length : 0")` instead
- No method calls on identifiers: `.map()`, `.filter()`, `.concat()` — wrap in `compute()`
- No arithmetic between identifiers: `a + b` — use `compute("a + b")`
- No bare component calls without assignment
- No HTML, no CSS, no `<script>` tags
- No `write(...)` tool calls — never write the app to a file, the openui fence IS the app
- No `curl | grep` or `curl | awk` — always pipe through `jq` or `python3` to produce valid JSON
