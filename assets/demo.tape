# Records assets/demo.gif with VHS (https://github.com/charmbracelet/vhs).
# Run it from the repository root, with elprovo on PATH:
#
#   make build && PATH="$PWD/bin:$PATH" vhs assets/demo.tape
#
# The commands run for real, so the GIF shows their real timing.
#
# VHS 0.12 writes no GIF with ffmpeg 9, and it reports no error. Then replace
# the Output line with `Output "<dir>/frames/"`, record, and join the frames:
#
#   ffmpeg -framerate 50 -i <dir>/frames/frame-text-%05d.png \
#     -framerate 50 -i <dir>/frames/frame-cursor-%05d.png \
#     -filter_complex "[0][1]overlay,fps=25,pad=iw+48:ih+48:24:24:color=0x171717,split[a][b];[a]palettegen=max_colors=64:stats_mode=diff[p];[b][p]paletteuse=dither=none:diff_mode=rectangle" \
#     assets/demo.gif

Output assets/demo.gif

Require elprovo
Require go

Set Shell "bash"
Set FontSize 18
Set Width 1240
Set Height 680
Set Padding 24
Set TypingSpeed 50ms

Hide
Type `cd "$(sh assets/demo-repo.sh)" && export GOCACHE="$(go env GOCACHE)" GOMODCACHE="$(go env GOMODCACHE)" && clear`
Enter
Sleep 2s
Show

Type "cat cart/coupon_test.go"
Sleep 500ms
Enter
Sleep 5s

Type "elprovo reproduce --base main cart/coupon_test.go"
Sleep 500ms
Enter
Wait+Screen@60s /Tests: 1 reproduced/
Sleep 5s

Hide
Type "clear"
Enter
Sleep 500ms
Show

Type "git switch -q coupon-once && elprovo replay --base main"
Sleep 500ms
Enter
Wait+Screen@90s /Other tests:/
Sleep 8s
