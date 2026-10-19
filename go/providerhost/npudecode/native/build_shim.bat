@echo off
rem Builds oa_npu_decode.dll with MSVC against the Windows XRT SDK headers
rem and import library. The DLL loads xrt_coreutil.dll from the NPU driver at
rem run time and needs nothing from the SDK then.
rem Usage: build_shim.bat <XRT SDK root, ...\xrt_sdk\xrt> <output folder>
setlocal
if "%~2"=="" (
  echo usage: build_shim.bat ^<XRT SDK root^> ^<output folder^>
  exit /b 2
)
set "XRT=%~1"
set "OUT=%~2"
if not exist "%XRT%\include\xrt\xrt_kernel.h" (
  echo %XRT% holds no XRT SDK headers
  exit /b 2
)
call "C:\Program Files\Microsoft Visual Studio\18\Community\VC\Auxiliary\Build\vcvars64.bat" >nul
if errorlevel 1 exit /b 1
if not exist "%OUT%" mkdir "%OUT%"
rem /Zc:__cplusplus: without it MSVC reports C++98 and XRT's any.h asks for
rem Boost. /MD: XRT's C++ API passes std::string across the DLL boundary, so
rem the shim shares the runtime xrt_coreutil.dll is built against.
cl /nologo /std:c++17 /Zc:__cplusplus /EHsc /O2 /MD /LD /W4 /I"%XRT%\include" "%~dp0oa_npu_decode.cpp" ^
   /Fo"%OUT%\\" /Fe"%OUT%\oa_npu_decode.dll" /link "%XRT%\lib\xrt_coreutil.lib"
exit /b %errorlevel%
