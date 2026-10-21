$ErrorActionPreference = 'Stop'
$repoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
# Pythonを起動せず、既存移行の固定SQLだけを抽出する。動的移行が追加された場合は黙って無視しない。
$parts = Get-ChildItem (Join-Path $repoRoot 'server\app\migrations\models\*.py') | Sort-Object { [int]($_.Name.Split('_')[0]) } | ForEach-Object {
    $raw = [IO.File]::ReadAllText($_.FullName).Replace("`r`n", "`n")
    $match = [regex]::Match($raw, '(?s)async def upgrade\([^\n]*\) -> str:\s*return """(.*?)"""')
    if (-not $match.Success) { throw "Migration requires a bootstrap implementation review: $($_.Name)" }
    '-- ' + $_.Name + "`n" + $match.Groups[1].Value
}
[IO.File]::WriteAllText((Join-Path $repoRoot 'server-go\internal\setup\schema.sql'), ($parts -join "`n"), [Text.UTF8Encoding]::new($false))
