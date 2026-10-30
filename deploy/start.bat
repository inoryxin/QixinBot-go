@echo off
rem QixinBot 启动脚本（供计划任务调用）
rem 已经在跑就直接退出，避免重复启动

tasklist /FI "IMAGENAME eq qixinbot.exe" 2>nul | find /I "qixinbot.exe" >nul
if not errorlevel 1 exit /b 0

cd /d "%~dp0"
if not exist data mkdir data
bin\qixinbot.exe -config config.toml >> data\stderr.log 2>&1
