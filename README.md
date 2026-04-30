# LAN File Transfer

A trusted-network file transfer app written in Go with an embedded web UI.

## Run

```sh
go run .
```

Or use the run script:

```sh
./run.sh
```

The server shares the directory where it is started. By default it listens on:

- Web/API: `32998`
- UDP discovery: `32997`

Use another port if needed:

```sh
go run . --port 33000
```

Skip automatic browser opening:

```sh
go run . --no-open
```

## Build Executables

Build Windows executables:

```sh
./scripts/build_windows.sh
```

Build macOS executables:

```sh
./scripts/build_macos.sh
```

Build Linux executables:

```sh
./scripts/build_linux.sh
```

Outputs are written to `dist/`. The Windows script creates `amd64` and `arm64` `.exe` files. The macOS script creates `amd64` and `arm64` binaries, and also creates a universal binary when `lipo` is available. The Linux script creates `amd64` and `arm64` binaries.

## Features

- PWA manifest and service worker so supported mobile browsers can install the web app.
- Installed/cached web app scans likely LAN ranges on the same port and reconnects to a reachable server automatically.
- Peer discovery with UDP broadcast and LAN probing.
- Canvas-based peer map.
- Browser-only visitors appear as disabled peers for file browsing, but files can still be sent to them and will download in that browser.
- Browser-only visitors choose a 4-letter display name and see themselves as the center "You" node.
- Local LAN IP display, excluding loopback addresses.
- Click a peer to view files.
- Browse local or remote current directories.
- Open and save UTF-8 text files in the built-in editor.
- Download files.
- Upload one or more files.
- Send and receive progress indicators while transfers are active.
- Drag and drop uploads into the current folder.
- Create folders on local or remote peers.
- Duplicate upload names are preserved with numbered suffixes.

## Troubleshooting

- PWA install support depends on the browser. Some mobile browsers require HTTPS for installable PWAs and may not offer installation from a plain `http://192.168...` LAN URL.
- `favicon.ico 404` in the browser console is harmless in old builds. Rebuild with the latest code to remove it.
- `502 Bad Gateway` while uploading to a peer means this device could not complete the HTTP request to the remote peer. Confirm the peer app is still running, both devices are on the same network, and Windows Firewall allows inbound private-network connections to the app or port `32998`.
- Large uploads use a 30 minute timeout. If an upload still times out quickly, the remote peer is probably unreachable rather than slow.

## Trust Model

This app intentionally has no password or authentication. Anyone who can reach the HTTP port on the same network can browse, download, upload, and create folders inside the directory where the server was started.

Run it only on networks and folders you trust.
