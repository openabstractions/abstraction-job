# The C++ binding of the job abstraction, as something another program can
# depend on.
#
#     include(FetchContent)
#     FetchContent_Declare(abstraction_job
#         GIT_REPOSITORY https://github.com/openabstractions/abstraction-job.git
#         GIT_TAG        <a commit sha>
#         GIT_SHALLOW    TRUE
#     )
#     FetchContent_MakeAvailable(abstraction_job)
#     target_link_libraries(your_target PRIVATE abstraction::job)
#
# Pin a sha rather than a branch. The record format is a cross-language
# contract, and an adopter tracking a branch could find the way its work is
# recorded altered by somebody else's commit. A sha makes adopting a change a
# decision somebody made.

cmake_minimum_required(VERSION 3.16)
project(abstraction_job LANGUAGES CXX VERSION 0.1.0)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)

if(CMAKE_SOURCE_DIR STREQUAL CMAKE_CURRENT_SOURCE_DIR)
    set(ABSTRACTION_JOB_IS_TOP_LEVEL ON)
else()
    set(ABSTRACTION_JOB_IS_TOP_LEVEL OFF)
endif()

option(ABSTRACTION_JOB_BUILD_TESTS "Build this layer's own tests" ${ABSTRACTION_JOB_IS_TOP_LEVEL})
option(ABSTRACTION_JOB_BUILD_TOOLS "Build jobctl, the conformance reader" ${ABSTRACTION_JOB_IS_TOP_LEVEL})

# nlohmann/json is the one dependency. Reused when the parent project already
# has it -- every adopter so far does -- so this never fetches a second copy.
if(NOT TARGET nlohmann_json::nlohmann_json)
    find_package(nlohmann_json 3.9 QUIET)
endif()
if(NOT TARGET nlohmann_json::nlohmann_json)
    include(FetchContent)
    FetchContent_Declare(nlohmann_json
        GIT_REPOSITORY https://github.com/nlohmann/json.git
        GIT_TAG        9cca280a4d0ccf0c08f47a99aa71d1b0e52f8d03  # v3.11.3
        GIT_SHALLOW    TRUE
    )
    set(JSON_BuildTests OFF CACHE INTERNAL "")
    set(JSON_Install OFF CACHE INTERNAL "")
    FetchContent_MakeAvailable(nlohmann_json)
endif()

add_library(abstraction_job STATIC
    cpp/src/record.cpp
    cpp/src/store.cpp
    cpp/src/discovery.cpp
)
target_include_directories(abstraction_job PUBLIC
    $<BUILD_INTERFACE:${CMAKE_CURRENT_SOURCE_DIR}/cpp/include>
    $<INSTALL_INTERFACE:include>
)
target_link_libraries(abstraction_job PUBLIC nlohmann_json::nlohmann_json)
# PUBLIC rather than relying on the directory-level CMAKE_CXX_STANDARD above:
# the headers use std::optional, so a consumer left at CMake's default of C++14
# fails on the first #include. The requirement has to travel with the target.
target_compile_features(abstraction_job PUBLIC cxx_std_17)
add_library(abstraction::job ALIAS abstraction_job)

# The discovery client, and it is deliberately NOT part of abstraction_job.
#
# docs/discovery-ipc.md puts discovery in the download layer and says nothing in
# `job` may depend on it. The C++ toolchain is here, so the code is here — but
# as its own target that abstraction_job does not link and that does not link
# abstraction_job, so the forbidden dependency cannot form by accident and
# moving these files to the download repository is a git mv and two lines.
add_library(abstraction_discovery STATIC
    cpp/src/discovery_client.cpp
)
target_include_directories(abstraction_discovery PUBLIC
    $<BUILD_INTERFACE:${CMAKE_CURRENT_SOURCE_DIR}/cpp/include>
    $<INSTALL_INTERFACE:include>
)
target_link_libraries(abstraction_discovery PUBLIC nlohmann_json::nlohmann_json)
# Same reason as abstraction_job above: these headers use std::optional, and a
# consumer left at CMake's default C++14 fails on the first #include.
target_compile_features(abstraction_discovery PUBLIC cxx_std_17)
add_library(abstraction::discovery ALIAS abstraction_discovery)

# jobctl exists so three implementations can be compared byte for byte, which is
# the only thing that has ever caught a cross-language disagreement here. It is
# not part of what an adopter links, which is why it is behind an option that
# defaults off when this is a subproject.
if(ABSTRACTION_JOB_BUILD_TOOLS)
    add_executable(jobctl cpp/cmd/jobctl.cpp)
    target_link_libraries(jobctl PRIVATE abstraction_job)
endif()

if(ABSTRACTION_JOB_BUILD_TESTS)
    enable_testing()
    add_executable(test_job_record cpp/test/test_job_record.cpp)
    target_link_libraries(test_job_record PRIVATE abstraction_job)
    add_test(NAME JobRecord COMMAND test_job_record)

    # This one stands up a real listener — a named pipe on Windows, a unix
    # socket elsewhere — so it needs threads.
    find_package(Threads REQUIRED)
    add_executable(test_discovery_client cpp/test/test_discovery_client.cpp)
    target_link_libraries(test_discovery_client PRIVATE abstraction_discovery Threads::Threads)
    add_test(NAME DiscoveryClient COMMAND test_discovery_client)
endif()
