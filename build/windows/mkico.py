#!/usr/bin/env python3
"""Pack the rendered icon PNGs into a multi-size Windows .ico.

Input:  icon-256.png / icon-48.png / icon-32.png / icon-16.png
        in build/ (their parent directory — that is where the render step
        writes them; this script lives in build/windows/ and an earlier
        version looked NEXT TO ITSELF, so it always failed with
        FileNotFoundError and left the .ico stale)
Output: build/windows/icon.ico (Vista+ PNG-compressed entries)

The PNGs are rendered from web/src/app/icon.svg by scripts/render-icons.ps1
(a headless Chromium screenshot, then a downscale). After packing, run:

  go run github.com/tc-hib/go-winres@v0.3.3 make --in build/winres.json --out cmd/desktop/rsrc

to refresh the .syso resources linked into the desktop executable.

Note that build/winres.json does NOT consume this .ico: it lists the PNGs
directly. The .ico is for Windows shortcuts and release artifacts.
"""
import os
import struct
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
# build/ — the directory the render step writes into.
ICON_DIR = os.path.dirname(HERE)
SIZES = [(256, "icon-256.png"), (48, "icon-48.png"), (32, "icon-32.png"), (16, "icon-16.png")]


def main() -> None:
    blobs = []
    for size, name in SIZES:
        # Accept either location: build/ is the pipeline's output, and looking
        # next to this script too keeps a hand-placed file working.
        path = os.path.join(ICON_DIR, name)
        if not os.path.exists(path):
            path = os.path.join(HERE, name)
        if not os.path.exists(path):
            sys.exit(f"missing {name}: expected {os.path.join(ICON_DIR, name)}\n"
                     f"run scripts/render-icons.ps1 first")
        with open(path, "rb") as f:
            blob = f.read()
        if blob[:8] != b"\x89PNG\r\n\x1a\n":
            sys.exit(f"{name} is not a PNG")
        blobs.append((size, blob))

    header = struct.pack("<HHH", 0, 1, len(blobs))
    entries = b""
    offset = 6 + 16 * len(blobs)
    data = b""
    for size, blob in blobs:
        # ICONDIRENTRY: width%256, height%256, colors, reserved, planes, bpp, size, offset
        entries += struct.pack("<BBBBHHII", size % 256, size % 256, 0, 0, 1, 32, len(blob), offset)
        data += blob
        offset += len(blob)

    # HERE is build/windows/, so the .ico belongs NEXT TO THIS SCRIPT.
    # An earlier version joined HERE + "windows" again and wrote
    # build/windows/windows/icon.ico, which is why build/windows/icon.ico
    # stayed stale for the whole project.
    out = os.path.join(HERE, "icon.ico")
    os.makedirs(os.path.dirname(out), exist_ok=True)
    with open(out, "wb") as f:
        f.write(header + entries + data)
    print(f"wrote {out} ({os.path.getsize(out)} bytes, {len(blobs)} sizes)")


if __name__ == "__main__":
    main()
