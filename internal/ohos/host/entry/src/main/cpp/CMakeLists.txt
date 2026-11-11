cmake_minimum_required(VERSION 3.5.0)
project(mygo_host)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)

set(NATIVERENDER_ROOT_PATH ${CMAKE_CURRENT_SOURCE_DIR})
include_directories(${NATIVERENDER_ROOT_PATH})

add_library(mygo_host SHARED host.cpp)
target_link_libraries(mygo_host PUBLIC
    libace_napi.z.so
    libace_ndk.z.so
    libhilog_ndk.z.so
    libnative_window.so
    libnative_buffer.so
    libnative_vsync.so
    libohinputmethod.so
    libohinput.so
)
