#Requires -Version 5.1
$ErrorActionPreference = "Stop"

$Repo = "stvnksslr/claude-code-litellm-plugin"
$BinaryName = "claude-code-litellm-plugin"
$InstallDir = "$env:LOCALAPPDATA\Programs\$BinaryName"

function Write-Info { param($Message) Write-Host "[INFO] $Message" -ForegroundColor Green }
function Write-Warn { param($Message) Write-Host "[WARN] $Message" -ForegroundColor Yellow }
function Write-Error { param($Message) Write-Host "[ERROR] $Message" -ForegroundColor Red; exit 1 }

function Get-Architecture {
    switch ($env:PROCESSOR_ARCHITECTURE) {
        "AMD64" { return "x86_64" }
        "ARM64" { return "arm64" }
        "x86"   { return "i386" }
        default { Write-Error "Unsupported architecture: $env:PROCESSOR_ARCHITECTURE" }
    }
}

function Get-LatestVersion {
    try {
        $release = Invoke-RestMethod -Uri "https://api.github.com/repos/$Repo/releases/latest" -UseBasicParsing
        return $release.tag_name
    } catch {
        Write-Error "Failed to fetch latest release version: $_"
    }
}

function Install-Binary {
    param (
        [string]$Arch,
        [string]$Version
    )

    $archiveName = "${BinaryName}_Windows_${Arch}.zip"
    $downloadUrl = "https://github.com/$Repo/releases/download/$Version/$archiveName"

    Write-Info "Downloading $archiveName..."

    $tempDir = New-Item -ItemType Directory -Path (Join-Path $env:TEMP ([System.Guid]::NewGuid().ToString()))
    $archivePath = Join-Path $tempDir $archiveName

    try {
        Invoke-WebRequest -Uri $downloadUrl -OutFile $archivePath -UseBasicParsing

        Write-Info "Extracting archive..."
        Expand-Archive -Path $archivePath -DestinationPath $tempDir -Force

        # Create install directory
        if (-not (Test-Path $InstallDir)) {
            New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
        }

        # Install binary
        $binaryPath = Join-Path $tempDir "$BinaryName.exe"
        Copy-Item -Path $binaryPath -Destination (Join-Path $InstallDir "$BinaryName.exe") -Force

        Write-Info "Installed $BinaryName to $InstallDir"
    } finally {
        Remove-Item -Path $tempDir -Recurse -Force -ErrorAction SilentlyContinue
    }
}

function Add-ToPath {
    $currentPath = [Environment]::GetEnvironmentVariable("Path", "User")

    if ($currentPath -notlike "*$InstallDir*") {
        $newPath = "$currentPath;$InstallDir"
        [Environment]::SetEnvironmentVariable("Path", $newPath, "User")
        $env:Path = "$env:Path;$InstallDir"
        Write-Info "Added $InstallDir to user PATH"
        Write-Warn "You may need to restart your terminal for PATH changes to take effect"
    } else {
        Write-Info "$InstallDir is already in PATH"
    }
}

function Set-ClaudeSettings {
    $claudeDir = Join-Path $env:USERPROFILE ".claude"
    $settingsFile = Join-Path $claudeDir "settings.json"

    if (-not (Test-Path $claudeDir)) {
        New-Item -ItemType Directory -Path $claudeDir -Force | Out-Null
    }

    $statusLineConfig = @{
        type = "command"
        command = "claude-code-litellm-plugin"
    }

    if (Test-Path $settingsFile) {
        try {
            $settings = Get-Content $settingsFile -Raw | ConvertFrom-Json
            $settings | Add-Member -NotePropertyName "statusLine" -NotePropertyValue $statusLineConfig -Force
            $settings | ConvertTo-Json -Depth 10 | Set-Content $settingsFile -Encoding UTF8
            Write-Info "Updated Claude settings in $settingsFile"
        } catch {
            Write-Warn "Failed to parse existing settings. Creating backup and new file."
            Copy-Item $settingsFile "$settingsFile.bak"
            $newSettings = @{ statusLine = $statusLineConfig }
            $newSettings | ConvertTo-Json -Depth 10 | Set-Content $settingsFile -Encoding UTF8
        }
    } else {
        $settings = @{ statusLine = $statusLineConfig }
        $settings | ConvertTo-Json -Depth 10 | Set-Content $settingsFile -Encoding UTF8
        Write-Info "Created Claude settings at $settingsFile"
    }
}

function Main {
    Write-Info "Installing $BinaryName..."

    $arch = Get-Architecture
    $version = Get-LatestVersion

    Write-Info "Detected: Windows $arch"
    Write-Info "Latest version: $version"

    Install-Binary -Arch $arch -Version $version
    Add-ToPath
    Set-ClaudeSettings

    Write-Host ""
    Write-Info "Installation complete!"

    # Check if required env vars are set
    $missingVars = @()
    if (-not $env:ANTHROPIC_BASE_URL -and -not $env:LITELLM_PROXY_URL) {
        $missingVars += "ANTHROPIC_BASE_URL or LITELLM_PROXY_URL"
    }
    if (-not $env:ANTHROPIC_AUTH_TOKEN -and -not $env:LITELLM_PROXY_API_KEY) {
        $missingVars += "ANTHROPIC_AUTH_TOKEN or LITELLM_PROXY_API_KEY"
    }

    if ($missingVars.Count -gt 0) {
        Write-Warn "Missing environment variables:"
        foreach ($var in $missingVars) {
            Write-Warn "  - $var"
        }
    }
}

Main
