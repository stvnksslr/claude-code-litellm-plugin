#Requires -Version 5.1
# Installs claude-code-litellm-plugin and wires it into Claude Code.
#
#   irm https://raw.githubusercontent.com/stvnksslr/claude-code-litellm-plugin/main/install.ps1 | iex
#
# Also install the VS Code extension into detected editors:
#   & ([scriptblock]::Create((irm https://raw.githubusercontent.com/stvnksslr/claude-code-litellm-plugin/main/install.ps1))) -Editors
#
# Environment overrides: LITELLM_PLUGIN_EDITORS=1, LITELLM_PLUGIN_VERSION=X.Y.Z,
# LITELLM_PLUGIN_INSTALL_DIR=<dir>, LITELLM_PLUGIN_NO_MODIFY_PATH=1
param(
    [switch]$Editors,
    [string]$Version = $env:LITELLM_PLUGIN_VERSION,
    [string]$InstallDir = $env:LITELLM_PLUGIN_INSTALL_DIR,
    [switch]$NoModifyPath
)

# Everything runs inside a script block so preferences don't leak into the
# caller's session, and failures `throw` instead of `exit` (which would close
# the PowerShell window when run via `irm | iex`).
& {
    $ErrorActionPreference = "Stop"
    # setup/doctor report problems via exit codes (2 = existing statusLine kept); on
    # pwsh 7.4+ don't let that turn into a terminating error.
    $PSNativeCommandUseErrorActionPreference = $false
    $ProgressPreference = "SilentlyContinue"   # Invoke-WebRequest is very slow with a progress bar on 5.1
    [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

    $Repo = "stvnksslr/claude-code-litellm-plugin"
    $BinaryName = "claude-code-litellm-plugin"
    if (-not $InstallDir) { $InstallDir = Join-Path $env:LOCALAPPDATA "Programs\$BinaryName" }
    if (-not $Editors -and $env:LITELLM_PLUGIN_EDITORS) { $Editors = $true }
    if (-not $NoModifyPath -and $env:LITELLM_PLUGIN_NO_MODIFY_PATH) { $NoModifyPath = $true }

    function Write-Info($Message) { Write-Host "[INFO] $Message" -ForegroundColor Green }
    function Write-Warn($Message) { Write-Host "[WARN] $Message" -ForegroundColor Yellow }

    $arch = switch ($env:PROCESSOR_ARCHITECTURE) {
        "AMD64" { "x86_64" }
        "ARM64" { "arm64" }
        "x86"   { "i386" }
        default { throw "Unsupported architecture: $env:PROCESSOR_ARCHITECTURE" }
    }

    if (-not $Version) {
        try {
            $Version = (Invoke-RestMethod -Uri "https://api.github.com/repos/$Repo/releases/latest" -UseBasicParsing).tag_name
        } catch {
            throw "Could not determine the latest release from GitHub: $_"
        }
    }

    Write-Info "Installing $BinaryName $Version for Windows $arch"

    $archive = "${BinaryName}_Windows_${arch}.zip"
    $checksums = "${BinaryName}_${Version}_checksums.txt"
    $base = "https://github.com/$Repo/releases/download/$Version"
    $tempDir = Join-Path ([System.IO.Path]::GetTempPath()) ([System.Guid]::NewGuid().ToString())
    New-Item -ItemType Directory -Path $tempDir | Out-Null

    try {
        Write-Info "Downloading $archive..."
        Invoke-WebRequest -Uri "$base/$archive" -OutFile (Join-Path $tempDir $archive) -UseBasicParsing
        Invoke-WebRequest -Uri "$base/$checksums" -OutFile (Join-Path $tempDir $checksums) -UseBasicParsing

        $want = Get-Content (Join-Path $tempDir $checksums) |
            ForEach-Object { $parts = $_ -split '\s+'; if ($parts.Count -ge 2 -and $parts[1].TrimStart('*') -eq $archive) { $parts[0] } } |
            Select-Object -First 1
        if (-not $want) { throw "$archive is not listed in $checksums" }
        $got = (Get-FileHash -Algorithm SHA256 -Path (Join-Path $tempDir $archive)).Hash
        if ($got.ToLower() -ne $want.ToLower()) { throw "Checksum mismatch for $archive (expected $want, got $got)" }
        Write-Info "Checksum verified"

        Expand-Archive -Path (Join-Path $tempDir $archive) -DestinationPath $tempDir -Force
        New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
        $bin = Join-Path $InstallDir "$BinaryName.exe"
        Copy-Item -Path (Join-Path $tempDir "$BinaryName.exe") -Destination $bin -Force
        Write-Info "Installed $bin"
    } finally {
        Remove-Item -Path $tempDir -Recurse -Force -ErrorAction SilentlyContinue
    }

    # PATH: lets `setup` write the bare command name (portable across Git Bash and
    # PowerShell, which Claude Code uses to run the status line).
    $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
    $entries = @($userPath -split ';' | Where-Object { $_ })
    if ($entries -notcontains $InstallDir) {
        if ($NoModifyPath) {
            Write-Warn "$InstallDir is not on your PATH"
        } else {
            [Environment]::SetEnvironmentVariable("Path", (($entries + $InstallDir) -join ';'), "User")
            Write-Info "Added $InstallDir to your user PATH (new terminals pick it up)"
        }
    }
    if (($env:Path -split ';') -notcontains $InstallDir -and -not $NoModifyPath) {
        $env:Path = "$env:Path;$InstallDir"
    }

    Write-Host ""
    $setupArgs = @("setup")
    if ($Editors) { $setupArgs += "--editors" }
    & $bin @setupArgs
    switch ($LASTEXITCODE) {
        0 { }
        2 { Write-Warn "Your existing statusLine was kept. To switch to this plugin: $BinaryName setup --force" }
        default { Write-Warn "setup did not complete; fix the issue above and run: $BinaryName setup" }
    }

    Write-Host ""
    Write-Info "Checking your configuration ($BinaryName doctor):"
    & $bin doctor
}
