#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

use std::{process::{Child, Command, Stdio}, thread, time::Duration, sync::{Arc, Mutex}, net::TcpListener, fs};
use tauri::Manager;

#[derive(Clone)]
struct BackendState {
    port: u16,
}

#[tauri::command]
fn get_backend_url(state: tauri::State<BackendState>) -> String {
    format!("http://127.0.0.1:{}", state.port)
}

fn spawn_backend() -> (Child, u16) {
    // Reserve a free port and release it so the child can bind to it
    let listener = TcpListener::bind("127.0.0.1:0").expect("failed to bind port 0");
    let port = listener.local_addr().unwrap().port();
    drop(listener);

    // Load environment variables from project .env for dev runs
    let mut extra_env: Vec<(String, String)> = Vec::new();
    if let Ok(cwd) = std::env::current_dir() {
        // Try likely locations relative to src-tauri
        let candidates = [
            cwd.join("../../.env"),
            cwd.join("../../../.env"),
            cwd.join(".env"),
        ];
        let mut content_opt = None;
        for p in candidates.iter() {
            if let Ok(c) = fs::read_to_string(p) {
                content_opt = Some(c);
                break;
            }
        }
        if let Some(content) = content_opt {
            for line in content.lines() {
                let line = line.trim();
                if line.is_empty() || line.starts_with('#') { continue; }
                if let Some((k, v)) = line.split_once('=') {
                    let key = k.trim().to_string();
                    let mut val = v.trim().to_string();
                    if (val.starts_with('"') && val.ends_with('"')) || (val.starts_with('\'') && val.ends_with('\'')) {
                        val = val[1..val.len()-1].to_string();
                    }
                    extra_env.push((key, val));
                }
            }
        }
    }

    // Start backend with the reserved PORT and .env variables
    let mut cmd = Command::new("../../backend/bin/crm-api");
    cmd.env("PORT", port.to_string())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    for (k, v) in extra_env {
        cmd.env(k, v);
    }
    let mut child = cmd.spawn().expect("failed to start backend");
    // Stream logs in background
    if let Some(mut out) = child.stdout.take() {
        std::thread::spawn(move || {
            use std::io::{BufRead, BufReader};
            let reader = BufReader::new(&mut out);
            for line in reader.lines().flatten() { println!("[backend] {}", line); }
        });
    }
    if let Some(mut err) = child.stderr.take() {
        std::thread::spawn(move || {
            use std::io::{BufRead, BufReader};
            let reader = BufReader::new(&mut err);
            for line in reader.lines().flatten() { eprintln!("[backend] {}", line); }
        });
    }

    // Poll health until ready (timeout ~5s)
    for _ in 0..100 {
        if let Ok(resp) = ureq::get(&format!("http://127.0.0.1:{}{}", port, "/health"))
            .timeout(Duration::from_millis(100))
            .call() {
            if resp.status() == 200 { break; }
        }
        thread::sleep(Duration::from_millis(100));
    }

    (child, port)
}

fn main() {
    tauri::Builder::default()
        .setup(|app| {
            // Spawn backend and wait
            let (child, port) = spawn_backend();
            app.manage(BackendState { port });
            println!("BACKEND_PORT={}", port);
            // Show window after short delay (backend health assumed)
            let window = app.get_webview_window("main").unwrap();
            window.show().ok();

            // Ensure backend is killed on app exit via on_window_event on the window
            let child_arc = Arc::new(Mutex::new(child));
            let child_for_close = child_arc.clone();
            window.on_window_event(move |event| {
                if let tauri::WindowEvent::CloseRequested { .. } = event {
                    if let Ok(mut ch) = child_for_close.lock() {
                        let _ = ch.kill();
                    }
                }
            });
            Ok(())
        })
        .invoke_handler(tauri::generate_handler![get_backend_url])
        .run(tauri::generate_context!())
        .expect("error while running tauri application");
}


