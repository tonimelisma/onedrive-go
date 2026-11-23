# W49 differential run: main 341d9820 against the fixed commit

Harness: `reports/W49-differential-test.go.txt` (one file, compiled unchanged into `git archive` exports of 341d9820 and of the commit at 33b6c8d1; the final commit d4590f38 differs from 33b6c8d1 only in comments, the spec and the message). Tree: A (a.txt, s/t.txt), B (b.txt). Steps: renA (A to A2), renS (s to s2), mvBA (B into the folder at A), mvSB (s into the folder at B), each once; mk (a new folder at the path the latest folder step left, at most twice); mv<f><o|n> (a.txt, t.txt or b.txt into the newest made folder, own or new name); bk<f> (a file back to the path its own last move left). Each sequence is run with the steps on this computer and again on OneDrive (the fake), then up to 3 syncs. Runs: `DW_OUT=... DW_FIVE=200 go test -count=1 -timeout 50m -run '^TestZZDiff$' ./internal/sync/` behind the heavy lock, no -v, no -race: test time main 1022 s, HEAD 799 s.

## Counts
- Sequences: all 466 of up to 4 steps, and 200 of the 2790 five-step ones (the 276 that make two folders again come first, in the order of seed 4949; the first 200 of them), so 666 sequences, 1332 runs per export.
- Both converge to the same trees: 1211.
- Main converges and HEAD does not: 0.
- Both converge to different trees: 82.
- HEAD converges and main does not: 0.
- Neither converges: 39.
- HEAD needs more syncs than main: 0. HEAD supersedes fewer actions: 119; more: 58.

Converges means a sync ended with nothing left within 3 syncs and both trees are equal. A second script recomputed from the step codes the tree the user left: all 1211 same-tree runs end with it, and in every one of the 82 HEAD ends with it and main does not.

## Classes
- Both converge, different trees (82): the intended fix. 81 move a file under its own name into the folder made at the path its folder left (a put-back); main moves it back into the moved folder, e.g. `here mvBA,mk,mvSB,mvbo`: main A/B/b.txt, HEAD B/b.txt. 1 is on OneDrive, `renA,mk,mvbn,renS,mk`: main ends with A2/nb.txt, HEAD with A/nb.txt, where OneDrive put it. 81 on this computer, 1 on OneDrive.
- Neither converges (39): all on this computer, the same cycle counts on both trees (12 or 15 cycle lines over 3 syncs), none caused by the commit. Each drops, on every pass, an entry leaving a moving folder for a new folder made at the path a moved folder left while that new folder can only be made after the moving folder moved: `mvBA,mk,*,renA` and `mvBA,mk,renA,*` (19; y moved into x, a new y made, an entry moved from x into it, x renamed: x's move waits for the entry, the entry for the new y, the new y for y's move, y's move to x2/y for x's), `renA,mk,mvSB,mvt*` and kin (10; s leaves the renamed x, t.txt leaves s for the new x: s's own move waits for t.txt), `mvSB,mk,mvBA,mvb*` and `mvBA,mk,mvSB,mk,mvb*` (8, b.txt from the moving B into a folder made at s's old path), `renA,mk,mvBA,mk,mvSB|mvan` (2). Named in the message's "Still dropped".
- HEAD supersedes more (58, all on OneDrive, all converge in one sync on both): a local move of a folder into a folder OneDrive made at the path another local move leaves (e.g. `renA,mk,mvBA,mvtn`: B to the new A) is planned in the first pass with no local create of the new folder (SQLite plans none while the old folder still holds the path) and no order against the folder move; on main it happens to run first, lands in the old folder and is moved again from A2/B in pass 2; here the hold of t.txt changes the timing, it runs after A's move and is superseded once. The plan of that move is main's; 133b1be0 supersedes in 53 of the 58 too. Seen and not fixed.
- HEAD supersedes fewer (119; 112 on this computer, 7 on OneDrive): mostly a file moved into the folder made at its folder's old path (102 under a new name), now moved from the rebased source in the first pass instead of failing once from the old one.
