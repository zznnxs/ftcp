@echo off
chcp 65001 > nul
setlocal enabledelayedexpansion

echo ================================
echo    TCP 内网穿透工具构建脚本
echo ================================

:: 检查 Go 环境
echo 检查 Go 环境...
go version >nul 2>&1
if %errorlevel% neq 0 (
    echo 错误：未检测到 Go 环境，请先安装 Go 1.24 或更高版本
    exit /b 1
)

:: 创建输出目录
if not exist "bin" (
    echo 创建输出目录...
    mkdir bin
)

:: 清理旧的构建文件
echo 清理旧的构建文件...
del /q bin\win\ftcpc.exe >nul 2>&1
del /q bin\linux\ftcps-amd64 >nul 2>&1

echo.
echo 正在构建 Windows 客户端...
go build -ldflags="-s -w" -o bin/win/ftcpc.exe client.go
if %errorlevel% neq 0 (
    echo 错误：Windows 客户端构建失败
    exit /b %errorlevel%
)
echo Windows 客户端构建成功

echo.
echo 正在构建 Linux 服务端...
set GOOS=linux
set GOARCH=amd64
go build -ldflags="-s -w" -o bin/linux/ftcps-amd64 server.go
set GOOS=
set GOARCH=
if %errorlevel% neq 0 (
    echo 错误：Linux 服务端构建失败
    exit /b %errorlevel%
)
echo Linux 服务端构建成功

echo.
echo ================================
echo    构建完成！
echo ================================
echo Windows 客户端: bin\win\ftcpc.exe
echo Windows 在任务计划程序增加 ftcpc.exe -server 127.0.0.1:7000 -id Windows11 -token xxxxx -map 8080:127.0.0.1:3389
echo ================================
echo Linux 服务端:   bin\linux\ftcps-amd64
echo Linux bin\linux\ 目录下文件拷贝到目標 Linux 服务器在运行:
echo sudo bash install.sh
echo ================================