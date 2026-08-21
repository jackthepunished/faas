#!/usr/bin/env python3
"""Floor checker for ship-blocking packages.

Reads Go cover profile files and computes statement coverage
per pkg/{fcvm,state,sched,gateway,vmmdgrpc,vmmdmount}. Exits 0
if all packages meet their floor, 1 otherwise.

Cover profile format (Go):
  github.com/onebox-faas/faas/pkg/fcvm/alloc.go:82.37,86.2 3 7
where columns are: path:start.col,end.col STMT_COUNT HIT_COUNT
"""
import re, sys

FLOORS = {
    # 1pp below the post-cluster-3/4 actual numbers — these
    # act as a regression tripwire, not aspirational targets.
    # Raise ONLY AFTER a subsequent coverage PR proves the +1pp.
    "pkg/fcvm":       46,
    "pkg/state":      42,
    "pkg/sched":      61,
    "pkg/gateway":    73,
    "pkg/vmmdgrpc":   46,
    "pkg/vmmdmount":  43,
}
REPO_PREFIX = "github.com/onebox-faas/faas/"


def main(paths):
    stats = {k: [0, 0] for k in FLOORS}  # [total_stmts, covered_stmts]
    for path in paths:
        with open(path) as f:
            for raw in f:
                if raw.startswith("mode:"):
                    continue
                parts = raw.rstrip("\n").split()
                if len(parts) < 3:
                    continue
                file_pos, stmts_str, hits_str = parts[0], parts[1], parts[2]
                colon = file_pos.find(":")
                if colon < 0:
                    continue
                file_ = file_pos[:colon]
                # Strip repo prefix so the path reduces to
                # pkg/foo/bar.go for matching against FLOORS keys.
                if file_.startswith(REPO_PREFIX):
                    file_ = file_[len(REPO_PREFIX):]
                if file_.endswith("_test.go"):
                    continue
                for pkg in FLOORS:
                    if file_.startswith(pkg + "/") or file_ == pkg:
                        if pkg == "pkg/state" and "/sqlc/" in file_:
                            continue  # exclude generated sqlc
                        n = int(stmts_str)
                        stats[pkg][0] += n
                        # A block of N statements is fully covered
                        # iff the block was executed at least once.
                        if int(hits_str) > 0:
                            stats[pkg][1] += n
                        break

    ok = True
    print("coverage-floor:")
    for pkg, floor in FLOORS.items():
        tot, hit = stats[pkg]
        if tot == 0:
            print(f"  {pkg}: (no statements in any shard — skip)")
            continue
        pct = hit * 100.0 / tot
        marker = "✓" if pct >= floor else "✗"
        if pct < floor:
            ok = False
        print(f"  {pkg}: {pct:.1f}% (floor ≥ {floor}%) {marker}")
    if not ok:
        print("coverage-floor: at least one package below floor")
        sys.exit(1)
    print("coverage-floor: all ship-blocking packages ≥ floor ✓")


if __name__ == "__main__":
    main(sys.argv[1:])
