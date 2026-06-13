//! Minimal smoke target that validates the Rust + hermetic_cc toolchain wiring
//! (docs/proposals/008). It builds a cdylib with a C-ABI export — exercising
//! rustc and the C linker (host by default, Zig under a libc-aware --platforms)
//! — and a unit test so `bazel test //...` covers the toolchain in CI. It has
//! no external crate dependencies on purpose: this proves the toolchain itself,
//! not crate_universe.

/// Exported with the C ABI so the link step produces a loadable shared object,
/// mirroring how real dynamic modules export their entrypoints.
#[no_mangle]
pub extern "C" fn aether_rust_toolchain_smoke() -> u32 {
    42
}

#[cfg(test)]
mod tests {
    use super::aether_rust_toolchain_smoke;

    #[test]
    fn returns_sentinel() {
        assert_eq!(aether_rust_toolchain_smoke(), 42);
    }
}
