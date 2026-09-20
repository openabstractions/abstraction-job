cmake_minimum_required(VERSION 3.16)

if(CMAKE_SOURCE_DIR STREQUAL CMAKE_CURRENT_SOURCE_DIR)
    project(abstraction_job LANGUAGES CXX VERSION 0.1.0)
    set(ABSTRACTION_JOB_STANDALONE ON)
else()
    set(ABSTRACTION_JOB_STANDALONE OFF)
endif()

include(GNUInstallDirs)
include(CMakePackageConfigHelpers)
# The generated acceptance vocabulary: header-only, standard library only.
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
