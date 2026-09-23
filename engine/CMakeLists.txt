cmake_minimum_required(VERSION 3.16)
project(worldcraft_engine CXX)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
if(NOT CMAKE_BUILD_TYPE)
  set(CMAKE_BUILD_TYPE Release)
endif()

# Two header-only dependencies, downloaded as single headers.
# Downloading the headers (instead of adding the libraries as CMake sub-projects)
# keeps this working on CMake 4.x, which rejects projects declaring compatibility
# with CMake older than 3.5.
set(VENDOR_DIR ${CMAKE_BINARY_DIR}/_headers)

function(fetch_header url dest)
  if(NOT EXISTS ${dest})
    message(STATUS "Downloading ${dest}")
    file(DOWNLOAD ${url} ${dest} STATUS status TLS_VERIFY ON)
    list(GET status 0 code)
    if(NOT code EQUAL 0)
      list(GET status 1 msg)
      file(REMOVE ${dest})
      message(FATAL_ERROR "Failed to download ${url}: ${msg}")
    endif()
  endif()
endfunction()

# nlohmann/json: JSON parsing and serialization
fetch_header(https://raw.githubusercontent.com/nlohmann/json/v3.11.3/single_include/nlohmann/json.hpp
             ${VENDOR_DIR}/nlohmann/json.hpp)
# cpp-httplib: tiny HTTP server, used only to serve /metrics
fetch_header(https://raw.githubusercontent.com/yhirose/cpp-httplib/v0.18.1/httplib.h
             ${VENDOR_DIR}/httplib.h)

# librdkafka C++ bindings, installed with your package manager (brew/apt)
find_package(PkgConfig REQUIRED)
pkg_check_modules(RDKAFKA REQUIRED IMPORTED_TARGET rdkafka++)
find_package(Threads REQUIRED)

add_executable(engine
  src/main.cpp
  src/world.cpp
  src/pathfinding.cpp
  src/kafka_io.cpp
  src/metrics.cpp)

target_compile_options(engine PRIVATE -Wall -Wextra)
target_include_directories(engine PRIVATE ${VENDOR_DIR})
target_link_libraries(engine PRIVATE PkgConfig::RDKAFKA Threads::Threads)

# Small self-test for A* so you can verify pathfinding without Kafka
add_executable(pathfinding_test src/pathfinding_test.cpp src/pathfinding.cpp)
