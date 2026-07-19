# hotlane README demo: edit -> push -> verified live -> rollback
Output docs/demo.gif

Set FontSize 15
Set Width 860
Set Height 480
Set Padding 18
Set Theme "Catppuccin Mocha"
Set TypingSpeed 40ms

Hide
Type "cd /private/tmp/claude-501/-Users-backup-Desktop-personal-orkestr/be1575d3-8402-42d9-b360-a48dd05a958d/scratchpad/demo && export PATH=/private/tmp/claude-501/-Users-backup-Desktop-personal-orkestr/be1575d3-8402-42d9-b360-a48dd05a958d/scratchpad:$PATH && clear"
Enter
Show

Type "curl localhost:7480/"
Enter
Sleep 1s
Type `sed -i '' 's/"v1"/"v2"/' server.js`
Enter
Sleep 500ms
Type "hotlane push"
Enter
Sleep 3.5s
Type "curl localhost:7480/"
Enter
Sleep 1.5s
Type "hotlane rollback"
Enter
Sleep 2s
Type "curl localhost:7480/"
Enter
Sleep 2.5s
