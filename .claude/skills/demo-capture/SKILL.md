---
name: demo-capture
description: Capture console screenshots and the demo transcript for docs/images using Playwright against the running stack.
---

# Demo capture

1. `make up && make seed`, then `make demo` to create audit rows and a pending approval.
2. `cd e2e && pnpm exec playwright test --project=screenshots` writes `docs/images/*.png` (375 and 1280 px, light and dark).
3. Check the images exist and are non-empty; never edit or fake a screenshot.
4. Reference images from `README.md` with alt text describing what they show.
