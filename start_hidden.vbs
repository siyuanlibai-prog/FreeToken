' FreeToken - 后台静默启动脚本
' 双击此文件即可在后台启动网关，不会弹出窗口。
' 脚本自动定位同目录下的 sensenova-gateway.exe。
Option Explicit

Dim fso, shell, exe, wd

Set fso = CreateObject("Scripting.FileSystemObject")
Set shell = CreateObject("WScript.Shell")

wd = Left(WScript.ScriptFullName, InStrRev(WScript.ScriptFullName, "\") - 1)
exe = wd & "\sensenova-gateway.exe"

If Not fso.FileExists(exe) Then
  MsgBox "网关程序不存在：" & vbCrLf & exe & vbCrLf & vbCrLf & _
         "请先运行 go build -o sensenova-gateway.exe .", 48, "FreeToken"
  WScript.Quit 1
End If

shell.CurrentDirectory = wd
shell.Run """" & exe & """", 0, False
