#!/usr/bin/env python3
"""Copy stdin to a file while enforcing a hard byte limit and fsyncing it."""

from __future__ import annotations

import os
import sys
from pathlib import Path

if len(sys.argv) != 3:
    raise SystemExit("usage: capped-copy.py OUTPUT MAX_BYTES")

output = Path(sys.argv[1])
limit = int(sys.argv[2])
written = 0
try:
    with output.open("wb") as destination:
        while chunk := sys.stdin.buffer.read(1024 * 1024):
            written += len(chunk)
            if written > limit:
                raise RuntimeError(f"output exceeded {limit} bytes")
            destination.write(chunk)
        destination.flush()
        os.fsync(destination.fileno())
except Exception:
    output.unlink(missing_ok=True)
    raise
