# hotlane README demo: edit -> push (verified + replayed) -> live -> rollback
#
# The recording performs REAL pushes against a real domain. Prereqs in
# the invoking shell (vhs inherits env; nothing secret appears on screen):
#   DEMO_DIR        checkout of the app repo matching the daemon's baseline
#   HOTLANE_DAEMON  https://demo.hotlane.dev
#   HOTLANE_TOKEN   the daemon's token
# The daemon serves the example demo-app (body = message.txt verbatim) at
# demo.hotlane.dev, replay buffer seeded (~50 GETs) so the push shows a
# real "replay N/N matched" line.
#
# Record:  vhs docs/demo.tape   (from the repo root)

Output docs/demo.gif

Set FontSize 17
Set Width 1080
Set Height 460
Set Padding 24
Set TypingSpeed 45ms
Set Theme { "background": "#16130f", "foreground": "#ece4d6", "cursor": "#ffa028", "black": "#352c22", "red": "#e0604a", "green": "#8bc47a", "yellow": "#ffa028", "blue": "#a3987f", "magenta": "#ffc668", "cyan": "#8bc47a", "white": "#ece4d6", "brightBlack": "#5c5040", "brightRed": "#e0604a", "brightGreen": "#8bc47a", "brightYellow": "#ffc668", "brightBlue": "#a3987f", "brightMagenta": "#ffc668", "brightCyan": "#8bc47a", "brightWhite": "#ece4d6" }

Hide
Type "cd $DEMO_DIR && clear"
Enter
Show

Type "curl https://demo.hotlane.dev/"
Enter
Sleep 1.5s

Type `echo "Ship it." > message.txt`
Enter
Sleep 800ms

Type "hotlane push"
Enter
Sleep 6.5s

Type "curl https://demo.hotlane.dev/"
Enter
Sleep 2s

Type "hotlane rollback"
Enter
Sleep 3s

Type "curl https://demo.hotlane.dev/"
Enter
Sleep 3.5s
