param([int]$Count = 3, [string]$Bench = '.', [switch]$TestsOnly, [switch]$Race)

# Run platform-independent paths without the existing Linux-only TProxy socket code.
$ErrorActionPreference = 'Stop'
$repo = Split-Path -Parent $PSScriptRoot
$scratch = Join-Path ([IO.Path]::GetTempPath()) ('tproxy-perf-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $scratch | Out-Null
try {
    foreach ($name in @('go.mod', 'go.sum', 'analytics.go', 'log.go', 'config.go', 'config.yaml', 'adblock.go', 'mcp_config.go', 'mcp_config_test.go', 'mcp_settings.go', 'mcp_settings_test.go', 'performance_test.go', 'analytics_test.go', 'log_test.go', 'adblock_test.go')) {
        $source = Join-Path $repo $name
        if (Test-Path -LiteralPath $source) { Copy-Item -LiteralPath $source -Destination $scratch }
    }
    New-Item -ItemType Directory -Path (Join-Path $scratch 'internal') | Out-Null
    Copy-Item -LiteralPath (Join-Path $repo 'internal/mcpauth') -Destination (Join-Path $scratch 'internal') -Recurse
    @'
package main
import "net/http"
var cfg *Config
func validateAuthToken(*http.Request) bool { return true }
'@ | Set-Content -LiteralPath (Join-Path $scratch 'support_test.go') -Encoding utf8
    Push-Location $scratch
    try {
        if ($Race) { & go test -race -count=1 ./... }
        elseif ($TestsOnly) { & go test -count=1 ./... }
        else { & go test -run '^$' -bench $Bench -benchmem -count $Count ./... }
        if ($LASTEXITCODE -ne 0) { throw "Go verification failed: $LASTEXITCODE" }
    } finally { Pop-Location }
} finally {
    $resolved = [IO.Path]::GetFullPath($scratch)
    $tempRoot = [IO.Path]::GetFullPath([IO.Path]::GetTempPath())
    if ($resolved.StartsWith($tempRoot, [StringComparison]::OrdinalIgnoreCase) -and (Split-Path -Leaf $resolved) -like 'tproxy-perf-*') {
        Remove-Item -LiteralPath $resolved -Recurse -Force
    }
}
