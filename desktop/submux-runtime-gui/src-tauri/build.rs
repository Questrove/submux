fn main() {
    let version = std::env::var("SUBMUX_VERSION").unwrap_or_else(|_| "dev".to_string());
    println!("cargo:rustc-env=SUBMUX_GUI_VERSION={version}");
    tauri_build::build()
}
