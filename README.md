# MediaLayer

MediaLayer makes a music and video library stored on cloud storage (Google
Drive, Proton Drive, iCloud Drive, SMB shares) feel local: desktop apps see a
mounted drive whose tags and cover art load instantly, while audio and video
stream from the cloud on demand. There is no server: the app and one or more
clouds are all a user needs.

Status: early spike. The architecture proposal lives outside this repository
for now; this repository currently holds the rclone spike that decides whether
MediaLayer is built on rclone in Go (see [docs/spike.md](docs/spike.md)).
