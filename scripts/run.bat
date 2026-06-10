@echo off
setlocal

set "PUBLIC_URL=https://agentmux.kinboy.wang"
set "ADDR=127.0.0.1:8081"
set "DATA=agentmux.db"
set "ROOT=%~dp0.."

if exist "%ROOT%\agentmux-hub.exe" (
  set "HUB_EXE=%ROOT%\agentmux-hub.exe"
) else if exist "%ROOT%\agentmux-hub-windows-amd64.exe" (
  set "HUB_EXE=%ROOT%\agentmux-hub-windows-amd64.exe"
) else if exist "%ROOT%\agentmux.exe" (
  "%ROOT%\agentmux.exe" hub --addr "%ADDR%" --data "%ROOT%\%DATA%" --public-url "%PUBLIC_URL%"
  goto :done
) else (
  echo agentmux hub executable not found.
  echo Put agentmux-hub.exe, agentmux-hub-windows-amd64.exe, or agentmux.exe in the repository or release root.
  exit /b 1
)

"%HUB_EXE%" --addr "%ADDR%" --data "%ROOT%\%DATA%" --public-url "%PUBLIC_URL%"

:done
endlocal
