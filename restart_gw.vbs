' FreeToken - one-click restart script (run as Administrator)
' Usage: double-click this file, click "Yes" on the UAC prompt.
' Action: force-stop the old gateway process, then relaunch with the latest sensenova-gateway.exe (hidden window).
' NOTE: all paths are derived from this script's own location.
Option Explicit

Dim exe, wd, fso, f, log, shell, ok, exec2

' derive own directory from the script's full path
wd  = Left(WScript.ScriptFullName, InStrRev(WScript.ScriptFullName, "\") - 1)
exe = wd & "\sensenova-gateway.exe"
log = wd & "\restart.log"

Set fso = CreateObject("Scripting.FileSystemObject")
If Not fso.FileExists(exe) Then
  MsgBox "Gateway executable not found:" & vbCrLf & exe, 48, "FreeToken - restart failed"
  WScript.Quit 1
End If

Set f = fso.CreateTextFile(log, True)
f.WriteLine "[restart] start " & Now

Set shell = CreateObject("WScript.Shell")

' 1) force-kill all old gateway processes
shell.Run "taskkill /F /IM sensenova-gateway.exe", 0, True
f.WriteLine "[restart] taskkill done"
WScript.Sleep 2000

' 2) confirm the port is released; retry if still occupied
ok = True
Set exec2 = shell.Exec("netstat -ano")
Do While Not exec2.StdOut.AtEndOfStream
  If InStr(exec2.StdOut.ReadLine, ":18888") > 0 Then ok = False
Loop
If Not ok Then
  f.WriteLine "[restart] port still busy, retry"
  shell.Run "taskkill /F /IM sensenova-gateway.exe", 0, True
  WScript.Sleep 2000
End If

' 3) relaunch the new executable (hidden window, do not wait)
shell.CurrentDirectory = wd
shell.Run """" & exe & """", 0, False
f.WriteLine "[restart] relaunched"
f.Close

MsgBox "Gateway restarted." & vbCrLf & vbCrLf & _
       "Refresh the admin page to see the latest changes." & vbCrLf & vbCrLf & _
       "Log: " & log, 64, "FreeToken - restart OK"

WScript.Quit 0
