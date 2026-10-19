<#
Builds OA's NPU decoder kit: the shim (build_shim.bat) and the design
(build_kernel.ps1) in a scratch folder, then copies the four kit files into
-Kit and writes kit.json, their SHA-256. -Kit is created private to this
account (owner this account, a protected DACL of this account and SYSTEM),
so its files pass identity ID-D2 as npudecode requires; an existing -Kit must already be private and is
emptied of kit files first. Compiling runs nothing on the NPU.

  & make_kit.ps1 -Toolchain <folder with oa_npu_env.ps1> -XrtSdk <...\xrt_sdk\xrt> -Kit <kit folder>
#>
param(
  [Parameter(Mandatory = $true)][string]$Toolchain,
  [Parameter(Mandatory = $true)][string]$XrtSdk,
  [Parameter(Mandatory = $true)][string]$Kit
)
$ErrorActionPreference = 'Stop'
$files = 'oa_npu_decode.dll', 'fixed3.xclbin', 'fixed3.insts', 'fixed3.json'
$sid = [System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value
$sddl = "O:${sid}D:P(A;OICI;FA;;;${sid})(A;OICI;FA;;;SY)"
if (-not (Test-Path $Kit)) {
  New-Item -ItemType Directory $Kit | Out-Null
  $acl = Get-Acl $Kit
  $acl.SetSecurityDescriptorSddlForm($sddl)
  Set-Acl $Kit $acl
}
$have = (Get-Acl $Kit).Sddl
if (-not $have.StartsWith("O:${sid}") -or $have -notmatch 'D:P') {
  throw "$Kit is not private to this account: $have"
}
$scratch = Join-Path $env:TEMP ("oa-npu-kit-" + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory $scratch | Out-Null
try {
  & (Join-Path $PSScriptRoot 'build_shim.bat') $XrtSdk $scratch
  if ($LASTEXITCODE -ne 0) { throw "build_shim.bat failed ($LASTEXITCODE)" }
  & (Join-Path $PSScriptRoot 'build_kernel.ps1') -Toolchain $Toolchain -Out $scratch | Out-Null
  $sums = [ordered]@{}
  foreach ($f in $files) {
    Copy-Item -Force (Join-Path $scratch $f) (Join-Path $Kit $f)
    $sums[$f] = (Get-FileHash (Join-Path $Kit $f) -Algorithm SHA256).Hash.ToLower()
  }
  $manifest = [ordered]@{ format = 'oa-npu-kit'; version = 1; sha256 = $sums } | ConvertTo-Json
  [System.IO.File]::WriteAllText((Join-Path $Kit 'kit.json'), $manifest + "`n")
  foreach ($f in $files + 'kit.json') {
    '{0}  {1}' -f (Get-FileHash (Join-Path $Kit $f) -Algorithm SHA256).Hash.ToLower(), $f
  }
} finally {
  Remove-Item -Recurse -Force $scratch
}
