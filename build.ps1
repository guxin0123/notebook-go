#Requires -Version 5.1
<#
.SYNOPSIS
    构建并打包 notebook 发布文件。

.DESCRIPTION
    默认构建 Windows amd64 和 Linux amd64 两个版本到 dist/，
    并分别压缩为 notebook-windows-amd64.zip 和 notebook-linux-amd64.tar.gz。
    可用 -OS / -Arch 选择其他目标，-OutputDir 修改输出目录。

.EXAMPLE
    .\build.ps1
    构建 Windows amd64、Linux amd64，并分别打包压缩。

.EXAMPLE
    .\build.ps1 -OS linux -Arch arm64
    只构建并打包 Linux arm64。
#>
[CmdletBinding()]
param(
    [ValidateSet('linux', 'windows', 'darwin')]
    [string[]]$OS = @('windows', 'linux'),

    [ValidateSet('amd64', 'arm64')]
    [string[]]$Arch = @('amd64'),

    [string]$OutputDir = 'dist'
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function New-ReleaseArchive {
    param(
        [string]$BinaryPath,
        [string]$ArchivePath,
        [string]$TargetOS
    )

    Remove-Item -LiteralPath $ArchivePath -Force -ErrorAction SilentlyContinue

    if ($TargetOS -eq 'windows') {
        Compress-Archive -LiteralPath $BinaryPath -DestinationPath $ArchivePath -CompressionLevel Optimal
        return
    }

    if (-not (Get-Command tar -ErrorAction SilentlyContinue)) {
        throw '未找到 tar 命令，无法创建 .tar.gz 压缩包。'
    }

    $directory = Split-Path -Parent $BinaryPath
    $binaryName = Split-Path -Leaf $BinaryPath
    $archiveName = Split-Path -Leaf $ArchivePath
    Push-Location $directory
    try {
        & tar -czf $archiveName $binaryName
        if ($LASTEXITCODE -ne 0) { throw "打包 $ArchivePath 失败" }
    } finally {
        Pop-Location
    }
}

$repoRoot = $PSScriptRoot
if (-not (Test-Path (Join-Path $repoRoot 'go.mod'))) {
    throw 'build.ps1 必须放在项目根目录（未找到 go.mod）。'
}
if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    throw '未找到 go 命令，请安装 Go 1.22 或更高版本并加入 PATH。'
}

Push-Location $repoRoot
$savedEnvironment = @{}
foreach ($name in @('CGO_ENABLED', 'GOOS', 'GOARCH')) {
    $savedEnvironment[$name] = [Environment]::GetEnvironmentVariable($name)
}
try {
    Write-Host ('Go: ' + (& go version))

    if (-not [System.IO.Path]::IsPathRooted($OutputDir)) {
        $OutputDir = Join-Path $repoRoot $OutputDir
    }
    New-Item -ItemType Directory -Force -Path $OutputDir | Out-Null

    $targets = New-Object System.Collections.Generic.List[object]
    foreach ($targetOS in ($OS | Select-Object -Unique)) {
        foreach ($targetArch in ($Arch | Select-Object -Unique)) {
            $baseName = "notebook-$targetOS-$targetArch"
            $binaryName = $baseName
            if ($targetOS -eq 'windows') { $binaryName += '.exe' }

            $binaryPath = Join-Path $OutputDir $binaryName
            if ($targetOS -eq 'windows') {
                $archivePath = Join-Path $OutputDir ($baseName + '.zip')
            } else {
                $archivePath = Join-Path $OutputDir ($baseName + '.tar.gz')
            }

            Write-Host "==> 构建 $targetOS/$targetArch -> $binaryPath"
            $env:CGO_ENABLED = '0'
            $env:GOOS = $targetOS
            $env:GOARCH = $targetArch
            & go build -trimpath -ldflags '-s -w' -o $binaryPath .
            if ($LASTEXITCODE -ne 0) { throw "构建 $targetOS/$targetArch 失败" }

            $targets.Add([pscustomobject]@{
                OS      = $targetOS
                Arch    = $targetArch
                Binary  = $binaryPath
                Archive = $archivePath
            })
        }
    }

    Write-Host ''
    Write-Host '==> 打包'
    foreach ($target in $targets) {
        Write-Host ("==> {0} -> {1}" -f $target.Binary, $target.Archive)
        New-ReleaseArchive -BinaryPath $target.Binary -ArchivePath $target.Archive -TargetOS $target.OS
    }

    Write-Host ''
    Write-Host '完成：'
    foreach ($target in $targets) {
        $archive = Get-Item $target.Archive
        Write-Host ('  ' + $archive.FullName)
        Write-Host ("    {0:N0} 字节" -f $archive.Length)
    }
} finally {
    Pop-Location
    foreach ($name in @('CGO_ENABLED', 'GOOS', 'GOARCH')) {
        $value = $savedEnvironment[$name]
        if ($null -eq $value) {
            Remove-Item "Env:$name" -ErrorAction SilentlyContinue
        } else {
            Set-Item "Env:$name" $value
        }
    }
}
