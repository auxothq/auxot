#!/bin/bash
set -e

# Initialize the gitagent workspace on first boot if SOUL.md is absent.
# On subsequent starts (or when the directory is bind-mounted with an existing
# workspace) this block is skipped and the agent resumes normally.
if [ ! -f /home/agent/SOUL.md ]; then
    echo "[auxot-agent] SOUL.md not found — running gitagent init"
    gitagent init
fi

exec auxot-agent "$@"
