# Placeholder icons

`48.png`, `128.png`, `256.png` and `512.png` are plain placeholders (a rounded
square with a ring), made for the `.deb` (scripts/desktop-deb.sh installs them
as `/usr/share/icons/hicolor/<size>x<size>/apps/chat.conch.desktop.png`). They
are original to this repository and under its licence, AGPL-3.0-only. They stay
until #753 gives Conch branding.

Made with ImageMagick, so that they can be made again:

    for s in 48 128 256 512; do
      convert -size ${s}x${s} xc:none -fill '#0f3d4a' \
        -draw "roundrectangle 0,0 $((s-1)),$((s-1)) $((s/5)),$((s/5))" \
        -fill '#f2c14e' -draw "circle $((s/2)),$((s/2)) $((s/2)),$((s*3/4))" \
        -fill '#0f3d4a' -draw "circle $((s*11/20)),$((s/2)) $((s*11/20)),$((s*5/8))" \
        -strip -define png:exclude-chunk=date,time PNG32:$s.png
    done
