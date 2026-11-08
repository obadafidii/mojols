//! Zed extension for Mojo, backed by mojols.
//!
//! The extension stays thin on purpose: it finds or downloads the `mojols`
//! binary and starts it. mojols itself discovers mojo-lsp-server, supervises
//! it, and provides every language feature, so a fix ships once for every
//! editor.

use std::fs;
use zed_extension_api::{self as zed, settings::LspSettings, LanguageServerId, Result};

const SERVER_ID: &str = "mojols";
const BINARY: &str = "mojols";
const RELEASES_REPO: &str = "tdadadavid/mojols";

struct MojolsExtension {
    cached_binary: Option<String>,
}

impl MojolsExtension {
    fn binary_path(&mut self, id: &LanguageServerId, worktree: &zed::Worktree) -> Result<String> {
        // 1. An explicit path in settings: lsp.mojols.binary.path
        if let Ok(settings) = LspSettings::for_worktree(SERVER_ID, worktree) {
            if let Some(path) = settings.binary.and_then(|b| b.path) {
                return Ok(expand_home(&path, worktree));
            }
        }
        // 2. mojols on the worktree's PATH (e.g. `go install` into ~/go/bin).
        if let Some(path) = worktree.which(BINARY) {
            return Ok(path);
        }
        // 3. A copy this extension downloaded earlier.
        if let Some(path) = &self.cached_binary {
            if fs::metadata(path).map(|m| m.is_file()).unwrap_or(false) {
                return Ok(path.clone());
            }
        }
        // 4. Download the latest release for this platform.
        match self.download(id) {
            Ok(path) => {
                self.cached_binary = Some(path.clone());
                Ok(path)
            }
            Err(err) => {
                zed::set_language_server_installation_status(
                    id,
                    &zed::LanguageServerInstallationStatus::Failed(err.clone()),
                );
                Err(format!(
                    "mojols not found ({err}).\n\
                     Install it with:\n  go install github.com/tdadadavid/mojols/server/cmd/mojols@latest\n\
                     or point Zed at a build:\n  \"lsp\": {{ \"mojols\": {{ \"binary\": {{ \"path\": \"/path/to/mojols\" }} }} }}"
                ))
            }
        }
    }

    fn download(&self, id: &LanguageServerId) -> Result<String> {
        zed::set_language_server_installation_status(
            id,
            &zed::LanguageServerInstallationStatus::CheckingForUpdate,
        );
        let release = zed::latest_github_release(
            RELEASES_REPO,
            zed::GithubReleaseOptions {
                require_assets: true,
                pre_release: false,
            },
        )?;
        let (os, arch) = zed::current_platform();
        let os = match os {
            zed::Os::Mac => "darwin",
            zed::Os::Linux => "linux",
            zed::Os::Windows => "windows",
        };
        let arch = match arch {
            zed::Architecture::Aarch64 => "arm64",
            zed::Architecture::X8664 => "amd64",
            zed::Architecture::X86 => return Err("32-bit x86 is not supported".into()),
        };
        let asset_name = format!("mojols_{os}_{arch}.tar.gz");
        let asset = release
            .assets
            .iter()
            .find(|a| a.name == asset_name)
            .ok_or_else(|| format!("release {} has no asset {asset_name}", release.version))?;

        let dir = format!("mojols-{}", release.version);
        let exe = if os == "windows" { "mojols.exe" } else { "mojols" };
        let path = format!("{dir}/{exe}");
        if !fs::metadata(&path).map(|m| m.is_file()).unwrap_or(false) {
            zed::set_language_server_installation_status(
                id,
                &zed::LanguageServerInstallationStatus::Downloading,
            );
            zed::download_file(&asset.download_url, &dir, zed::DownloadedFileType::GzipTar)?;
            zed::make_file_executable(&path)?;
            // Remove older downloaded versions.
            if let Ok(entries) = fs::read_dir(".") {
                for entry in entries.flatten() {
                    let name = entry.file_name().to_string_lossy().to_string();
                    if name.starts_with("mojols-") && name != dir {
                        let _ = fs::remove_dir_all(entry.path());
                    }
                }
            }
        }
        zed::set_language_server_installation_status(
            id,
            &zed::LanguageServerInstallationStatus::None,
        );
        Ok(path)
    }
}

fn expand_home(path: &str, worktree: &zed::Worktree) -> String {
    if let Some(rest) = path.strip_prefix("~/") {
        if let Some((_, home)) = worktree.shell_env().into_iter().find(|(k, _)| k == "HOME") {
            return format!("{home}/{rest}");
        }
    }
    path.to_string()
}

impl zed::Extension for MojolsExtension {
    fn new() -> Self {
        Self {
            cached_binary: None,
        }
    }

    fn language_server_command(
        &mut self,
        id: &LanguageServerId,
        worktree: &zed::Worktree,
    ) -> Result<zed::Command> {
        let command = self.binary_path(id, worktree)?;
        let mut args = vec!["serve".to_string()];
        // The worktree's shell environment carries PATH from direnv/pixi, which
        // is how mojols finds mojo-lsp-server.
        let mut env = worktree.shell_env();
        if let Ok(settings) = LspSettings::for_worktree(SERVER_ID, worktree) {
            if let Some(binary) = settings.binary {
                if let Some(extra) = binary.arguments {
                    args.extend(extra);
                }
                if let Some(extra) = binary.env {
                    env.extend(extra);
                }
            }
        }
        Ok(zed::Command { command, args, env })
    }

    fn language_server_initialization_options(
        &mut self,
        id: &LanguageServerId,
        worktree: &zed::Worktree,
    ) -> Result<Option<zed::serde_json::Value>> {
        if id.as_ref() != SERVER_ID {
            return Ok(None);
        }
        LspSettings::for_worktree(SERVER_ID, worktree).map(|s| s.initialization_options)
    }

    fn language_server_workspace_configuration(
        &mut self,
        id: &LanguageServerId,
        worktree: &zed::Worktree,
    ) -> Result<Option<zed::serde_json::Value>> {
        if id.as_ref() != SERVER_ID {
            return Ok(None);
        }
        LspSettings::for_worktree(SERVER_ID, worktree).map(|s| s.settings)
    }
}

zed::register_extension!(MojolsExtension);
