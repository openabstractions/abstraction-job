cmake_minimum_required(VERSION 3.16)

if(CMAKE_SOURCE_DIR STREQUAL CMAKE_CURRENT_SOURCE_DIR)
    project(abstraction_job LANGUAGES CXX VERSION 0.1.0)
    set(ABSTRACTION_JOB_STANDALONE ON)
else()
    set(ABSTRACTION_JOB_STANDALONE OFF)
endif()

include(GNUInstallDirs)
include(CMakePackageConfigHelpers)
option(ABSTRACTION_JOB_BUILD_LEGACY "Build the legacy embedded job store" ON)
# Generated acceptance vocabulary is independent of the legacy store and CAS.
add_library(abstraction_job_acceptance INTERFACE)
add_library(abstraction::job_acceptance ALIAS abstraction_job_acceptance)
set_target_properties(abstraction_job_acceptance PROPERTIES EXPORT_NAME job_acceptance)
target_compile_features(abstraction_job_acceptance INTERFACE cxx_std_17)
target_include_directories(abstraction_job_acceptance INTERFACE
    $<BUILD_INTERFACE:${CMAKE_CURRENT_SOURCE_DIR}>
    $<INSTALL_INTERFACE:${CMAKE_INSTALL_INCLUDEDIR}>)
install(TARGETS abstraction_job_acceptance EXPORT abstraction_job_acceptanceTargets)
install(DIRECTORY abstraction/job/acceptance DESTINATION ${CMAKE_INSTALL_INCLUDEDIR}/abstraction/job)
set(acceptance_config_dir ${CMAKE_INSTALL_LIBDIR}/cmake/abstraction_job_acceptance)
install(EXPORT abstraction_job_acceptanceTargets NAMESPACE abstraction:: DESTINATION ${acceptance_config_dir})
configure_package_config_file(cmake/abstraction_job_acceptanceConfig.cmake.in
    ${CMAKE_CURRENT_BINARY_DIR}/abstraction_job_acceptanceConfig.cmake
    INSTALL_DESTINATION ${acceptance_config_dir})
write_basic_package_version_file(${CMAKE_CURRENT_BINARY_DIR}/abstraction_job_acceptanceConfigVersion.cmake
    VERSION 0.1.0 COMPATIBILITY SameMinorVersion)
install(FILES ${CMAKE_CURRENT_BINARY_DIR}/abstraction_job_acceptanceConfig.cmake
    ${CMAKE_CURRENT_BINARY_DIR}/abstraction_job_acceptanceConfigVersion.cmake DESTINATION ${acceptance_config_dir})
if(NOT ABSTRACTION_JOB_BUILD_LEGACY)
    return()
endif()

find_package(Threads REQUIRED)

option(ABSTRACTION_BUILD_TESTS "Build the abstraction's own tests"
    ${ABSTRACTION_JOB_STANDALONE})

# Three ways a sibling layer can already be here, and no fourth: a superproject
# added it, an install of it is on CMAKE_PREFIX_PATH, or its checkout is beside
# ours. Nothing is fetched -- a configure step that reaches the network is the
# thing this build refuses to hand downstream.
macro(abstraction_need name)
    if(NOT TARGET abstraction::${name})
        find_package(abstraction_${name} CONFIG QUIET)
        if(TARGET abstraction::${name})
            set_property(GLOBAL APPEND PROPERTY
                ABSTRACTION_PACKAGE_DEPS abstraction_${name})
        endif()
    endif()
    if(NOT TARGET abstraction::${name})
        foreach(d ../../${name}/cpp ../../abstraction-${name}/cpp)
            if(EXISTS ${CMAKE_CURRENT_SOURCE_DIR}/${d}/CMakeLists.txt)
                add_subdirectory(${CMAKE_CURRENT_SOURCE_DIR}/${d} abstraction_${name}-build)
                get_property(_have GLOBAL PROPERTY ABSTRACTION_SOURCE_TARGETS)
                if(NOT "abstraction_${name}" IN_LIST _have)
                    set_property(GLOBAL APPEND PROPERTY
                        ABSTRACTION_SOURCE_TARGETS abstraction_${name})
                    set_property(GLOBAL APPEND PROPERTY
                        ABSTRACTION_SOURCE_INCLUDES ${CMAKE_CURRENT_SOURCE_DIR}/${d}/include/)
                endif()
                break()
            endif()
        endforeach()
    endif()
    if(NOT TARGET abstraction::${name})
        message(FATAL_ERROR "abstraction_${name} not found. Install it and put its "
            "prefix on CMAKE_PREFIX_PATH, or clone abstraction-${name} beside this "
            "repository. Nothing is downloaded for you.")
    endif()
endmacro()

abstraction_need(cas)
abstraction_need(watch)

# The document format the record is written in. Its own target rather than a
# source file inside abstraction_job, because abstraction_discovery in the
# download layer must not link abstraction_job and both need to read JSON.
add_library(abstraction_json STATIC src/json.cpp)
target_include_directories(abstraction_json PUBLIC
    $<BUILD_INTERFACE:${CMAKE_CURRENT_SOURCE_DIR}/include>
    $<INSTALL_INTERFACE:${CMAKE_INSTALL_INCLUDEDIR}>
)
target_compile_features(abstraction_json PUBLIC cxx_std_17)
set_target_properties(abstraction_json PROPERTIES EXPORT_NAME json)
add_library(abstraction::json ALIAS abstraction_json)

# The store's own layout, header-only and separate from abstraction_job on
# purpose: the download header has to know which paths under a store root
# belong to the store, and taking a parser and a static library for a few
# string rules is the wrong trade.
add_library(abstraction_job_layout INTERFACE)
target_include_directories(abstraction_job_layout INTERFACE
    $<BUILD_INTERFACE:${CMAKE_CURRENT_SOURCE_DIR}/include>
    $<INSTALL_INTERFACE:${CMAKE_INSTALL_INCLUDEDIR}>
)
set_target_properties(abstraction_job_layout PROPERTIES EXPORT_NAME job_layout)
add_library(abstraction::job_layout ALIAS abstraction_job_layout)

add_library(abstraction_job STATIC
    src/awake.cpp
    src/record.cpp
    src/ranges.cpp
    src/store.cpp
    src/discovery.cpp
)
target_include_directories(abstraction_job PUBLIC
    $<BUILD_INTERFACE:${CMAKE_CURRENT_SOURCE_DIR}/include>
    $<INSTALL_INTERFACE:${CMAKE_INSTALL_INCLUDEDIR}>
)
# Namespaced names on purpose: abstraction::cas is an ALIAS when the layer came
# from source and an IMPORTED target when it came from an install, and only the
# namespaced name is spelled the same in both cases.
target_link_libraries(abstraction_job PUBLIC
    abstraction::json abstraction::watch abstraction::cas)
# nudge() opens an AF_UNIX socket, and on Windows that is Winsock. Without this
# every program linking this library fails at LINK with six unresolved imports —
# true for as long as discovery.cpp has been in the target, and unfound because
# no gate run on a machine without cmake ever reached a linker.
target_link_libraries(abstraction_job PUBLIC $<$<PLATFORM_ID:Windows>:ws2_32>)
# PUBLIC rather than a directory-level CMAKE_CXX_STANDARD: the headers use
# std::optional, so a consumer left at CMake's default of C++14 fails on the
# first #include. The requirement has to travel with the target.
target_compile_features(abstraction_job PUBLIC cxx_std_17)
set_target_properties(abstraction_job PROPERTIES EXPORT_NAME job)
add_library(abstraction::job ALIAS abstraction_job)

if(ABSTRACTION_BUILD_TESTS)
    enable_testing()

    add_executable(test_json test/test_json.cpp)
    target_link_libraries(test_json PRIVATE abstraction_json)
    add_test(NAME Json COMMAND test_json)

    add_executable(test_job_record test/test_job_record.cpp)
    target_link_libraries(test_job_record PRIVATE abstraction_job)
    add_test(NAME JobRecord COMMAND test_job_record)

    add_executable(test_job_store test/test_job_store.cpp)
    target_link_libraries(test_job_store PRIVATE abstraction_job Threads::Threads)
    add_test(NAME JobStore COMMAND test_job_store)

    # The range fixture is one file compared byte for byte by all three
    # implementations. Its directory is compiled in rather than looked for: a
    # test binary's working directory is wherever it was run from, and a
    # relative path finds the fixture only by luck.
    add_executable(test_ranges test/test_ranges.cpp)
    target_link_libraries(test_ranges PRIVATE abstraction_job)
    target_compile_definitions(test_ranges
        PRIVATE ABSTRACTION_JOB_TESTDATA="${CMAKE_CURRENT_SOURCE_DIR}/../testdata")
    add_test(NAME Ranges COMMAND test_ranges)
endif()

if(ABSTRACTION_JOB_STANDALONE)
    include(CMakePackageConfigHelpers)
    set(ABSTRACTION_JOB_CMAKEDIR ${CMAKE_INSTALL_LIBDIR}/cmake/abstraction_job)

    # Whatever came in from source travels in this package: a target linked by
    # an exported target and exported nowhere makes an install that cannot be
    # consumed, and CMake says so at generate time.
    get_property(ABSTRACTION_JOB_EXTRA GLOBAL PROPERTY ABSTRACTION_SOURCE_TARGETS)
    get_property(ABSTRACTION_JOB_EXTRA_INC GLOBAL PROPERTY ABSTRACTION_SOURCE_INCLUDES)

    # A sibling that came from an install stays an install for our consumers
    # too, so the config file asks for exactly the packages this build used.
    get_property(ABSTRACTION_JOB_PKGS GLOBAL PROPERTY ABSTRACTION_PACKAGE_DEPS)
    set(ABSTRACTION_JOB_DEPS "")
    foreach(p ${ABSTRACTION_JOB_PKGS})
        string(APPEND ABSTRACTION_JOB_DEPS "find_dependency(${p} CONFIG)\n")
    endforeach()

    install(TARGETS
        abstraction_json abstraction_job_layout abstraction_job
        ${ABSTRACTION_JOB_EXTRA}
        EXPORT abstraction_jobTargets
        ARCHIVE DESTINATION ${CMAKE_INSTALL_LIBDIR}
        LIBRARY DESTINATION ${CMAKE_INSTALL_LIBDIR}
        RUNTIME DESTINATION ${CMAKE_INSTALL_BINDIR})
    install(DIRECTORY include/ ${ABSTRACTION_JOB_EXTRA_INC}
        DESTINATION ${CMAKE_INSTALL_INCLUDEDIR})
    install(EXPORT abstraction_jobTargets
        FILE abstraction_jobTargets.cmake
        NAMESPACE abstraction::
        DESTINATION ${ABSTRACTION_JOB_CMAKEDIR})

    configure_package_config_file(cmake/abstraction_jobConfig.cmake.in
        ${CMAKE_CURRENT_BINARY_DIR}/abstraction_jobConfig.cmake
        INSTALL_DESTINATION ${ABSTRACTION_JOB_CMAKEDIR})
    write_basic_package_version_file(
        ${CMAKE_CURRENT_BINARY_DIR}/abstraction_jobConfigVersion.cmake
        COMPATIBILITY SameMinorVersion)
    install(FILES
        ${CMAKE_CURRENT_BINARY_DIR}/abstraction_jobConfig.cmake
        ${CMAKE_CURRENT_BINARY_DIR}/abstraction_jobConfigVersion.cmake
        DESTINATION ${ABSTRACTION_JOB_CMAKEDIR})
else()
    set_property(GLOBAL APPEND PROPERTY ABSTRACTION_SOURCE_TARGETS
        abstraction_json abstraction_job_layout abstraction_job)
    set_property(GLOBAL APPEND PROPERTY ABSTRACTION_SOURCE_INCLUDES
        ${CMAKE_CURRENT_SOURCE_DIR}/include/)
endif()
