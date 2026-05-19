#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from core.protocol_replay import ProtocolReplayConfig, replay_recording  # noqa: E402


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Convert a browser recording into protocol steps and optionally replay them.")
    parser.add_argument("recording")
    parser.add_argument("--live", action="store_true", help="Actually send protocol requests. Default only writes plan.")
    parser.add_argument("--output", default="")
    parser.add_argument("--resource-types", default="document,xhr,fetch")
    parser.add_argument("--timeout", type=int, default=30)
    parser.add_argument("--max-steps", type=int, default=0)
    args = parser.parse_args(argv)

    resource_types = {item.strip() for item in args.resource_types.split(",") if item.strip()}
    result = replay_recording(
        ProtocolReplayConfig(
            recording_path=args.recording,
            output_path=args.output,
            live=args.live,
            resource_types=resource_types,
            timeout=args.timeout,
            max_steps=args.max_steps,
        )
    )
    print(json.dumps({
        "output": result.get("output_path"),
        "live": result.get("live"),
        "steps": len((result.get("plan") or {}).get("steps") or []),
        "responses": len(result.get("responses") or []),
    }, ensure_ascii=False, indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
