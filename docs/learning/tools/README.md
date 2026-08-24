# Session-mining tools

Scripts to mine your own pi sessions (`~/.pi/agent/sessions/--Users-sahil-Projects-bhatti--/`)
for learning. The sessions are the record of every problem you solved building
bhatti — 139 sessions, ~232MB, 2,157 of your own prompts. These let you pull a
topic's history back out.

| Script | Does | Example |
|--------|------|---------|
| `extract.py` | Build `index.json`: per-session timestamp, size, message/tool counts, first prompt, all user prompts. | `python3 extract.py` |
| `search.py` | Find which sessions discuss a topic (ranked by # of matching turns). OR across terms. | `python3 search.py balloon deflate_on_oom` |
| `dump.py` | Dump one session's turns. Modes: `u`=user, `t`=assistant text, `k`=thinking, `c`=tool calls. | `python3 dump.py <file>.jsonl utk` |
| `users.py` | Dump just the user prompts of one session (the problem arc, compact). | `python3 users.py <file>.jsonl` |

**Workflow for studying a topic:** `search.py <topic>` → pick the top session →
`users.py <file>` to see the problem arc → `dump.py <file> utk` to read the full
reasoning. Cross-reference with the curriculum module and the actual code.

Note: `dump.py`/`users.py` resolve files relative to the sessions dir, so pass
just the `.jsonl` filename. Edit `SESS_DIR` at the top if your project path
differs.
