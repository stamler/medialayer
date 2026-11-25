# Working in this repository

- Every code change gets an independent review by a separate reviewer agent
  before it is pushed. Fix or answer each finding first.
- Push reviewed changes straight to `main`; don't open pull requests unless
  asked. Keep each commit small and focused.
- The spike code lives in `backend/medialayer` (an rclone overlay backend) and
  `cmd/mlspike` (an rclone binary with that backend built in). Run
  `go test ./...` and `go vet ./...` before pushing.
- rclone is a pinned Go module dependency. Do not fork it; send generally
  useful changes upstream.
