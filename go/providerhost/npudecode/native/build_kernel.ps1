<#
Compiles OA's NPU decoder design (fixed3_design.py, fixed3_decode.cc) into
fixed3.xclbin, fixed3.insts and fixed3.json with the IRON toolchain
(mlir-aie 1.4.3, Peano), then copies them into -Out. Compiling never runs
anything on the NPU.

  & build_kernel.ps1 -Toolchain <folder with oa_npu_env.ps1> -Out <folder> [-Units 64] [-Cols 8]

The compiler works in -Work, default %LOCALAPPDATA%\oa-npu-cache\oa-fixed3:
Peano's linker script splits paths at spaces, so that path has none.
#>
param(
  [Parameter(Mandatory = $true)][string]$Toolchain,
  [Parameter(Mandatory = $true)][string]$Out,
  [string]$Work = (Join-Path $env:LOCALAPPDATA 'oa-npu-cache\oa-fixed3'),
  [int]$Units = 64,
  [int]$Cols = 8
)
$ErrorActionPreference = 'Stop'
if ($Work.Contains(' ')) { throw "$Work has a space, which Peano's linker script splits" }
. (Join-Path $Toolchain 'oa_npu_env.ps1')
& python (Join-Path $PSScriptRoot 'fixed3_design.py') --out $Work --units $Units --cols $Cols
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
New-Item -ItemType Directory -Force $Out | Out-Null
foreach ($f in 'fixed3.xclbin', 'fixed3.insts', 'fixed3.json') {
  Copy-Item -Force (Join-Path $Work $f) (Join-Path $Out $f)
}
Get-ChildItem $Out -Filter 'fixed3.*' | ForEach-Object {
  '{0}  {1}  {2} bytes' -f (Get-FileHash $_.FullName -Algorithm SHA256).Hash.ToLower(), $_.Name, $_.Length
}
