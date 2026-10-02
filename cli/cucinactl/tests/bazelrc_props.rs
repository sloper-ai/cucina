// SPDX-License-Identifier: FSL-1.1-ALv2

//! Property tests for `.bazelrc` generation (UC9, R-DATA-1, §10.2, R-RE-1,
//! R-XPLAT-2(d), R-XPLAT-4, R-AUTH-8) over the embedded `platforms/targets.json`:
//! every generated flag exists in Bazel 9.2 (or is an exec platform's build
//! setting), only `startup` and `build` lines with `startup` first, the credential
//! helper is host-scoped, platform flags and exec-platform ordering follow the
//! rules, per-target extras (exec-platform flags, MSVC EULA, Windows test env,
//! scaled test timeouts, Windows-client lines) appear exactly where they must, and
//! every line survives Bazel's rc tokenizer (quoting of Windows paths).

mod support;

use std::collections::{BTreeSet, HashMap};

use cucinactl::bazelrc::{self, Bazelrc, Labels, Line, Mode, Os, Request};
use cucinactl::catalog::Catalog;
use proptest::prelude::*;

type Allow = BTreeSet<(String, String, bool)>;

fn allowlist() -> &'static Allow {
    static ALLOW: std::sync::OnceLock<Allow> = std::sync::OnceLock::new();
    ALLOW.get_or_init(load_allowlist)
}

fn load_allowlist() -> Allow {
    let path = support::repo_root().join("cli/cucinactl/testdata/bazel-9.2-flags.txt");
    std::fs::read_to_string(path)
        .expect("flag allow-list")
        .lines()
        .filter(|l| !l.starts_with('#') && !l.trim().is_empty())
        .map(|l| {
            let mut it = l.split_whitespace();
            let scope = it.next().unwrap().to_string();
            let name = it.next().unwrap().to_string();
            let is_bool = it.next() == Some("bool");
            (scope, name, is_bool)
        })
        .collect()
}

/// Bazel's rc-line tokenizer: whitespace separates tokens; single quotes are
/// literal; double quotes allow backslash escapes; a backslash outside quotes
/// escapes the next character.
fn tokenize(line: &str) -> Vec<String> {
    let (mut out, mut cur, mut has) = (Vec::new(), String::new(), false);
    let mut chars = line.chars();
    while let Some(c) = chars.next() {
        match c {
            '\'' => {
                has = true;
                for d in chars.by_ref() {
                    if d == '\'' {
                        break;
                    }
                    cur.push(d);
                }
            }
            '"' => {
                has = true;
                while let Some(d) = chars.next() {
                    match d {
                        '"' => break,
                        '\\' => cur.extend(chars.next()),
                        _ => cur.push(d),
                    }
                }
            }
            '\\' => {
                has = true;
                cur.extend(chars.next());
            }
            c if c.is_whitespace() => {
                if has {
                    out.push(std::mem::take(&mut cur));
                    has = false;
                }
            }
            c => {
                has = true;
                cur.push(c);
            }
        }
    }
    if has {
        out.push(cur);
    }
    out
}

fn catalog() -> &'static Catalog {
    static CATALOG: std::sync::OnceLock<Catalog> = std::sync::OnceLock::new();
    CATALOG.get_or_init(|| Catalog::embedded().expect("embedded catalog"))
}

fn request_strategy() -> impl Strategy<Value = Request> {
    let cat = catalog();
    let targets: Vec<(String, Vec<String>)> = cat
        .targets
        .targets
        .iter()
        .filter(|t| !t.excluded)
        .map(|t| {
            let pools = t
                .exec_platforms
                .iter()
                .map(|n| cat.exec_platform(n).unwrap().pool.clone())
                .collect();
            (t.name.clone(), pools)
        })
        .collect();
    let native_pools: HashMap<&'static str, Vec<String>> = [Os::Linux, Os::Windows, Os::Macos]
        .into_iter()
        .map(|os| {
            let pools = cat
                .targets
                .exec_platforms
                .iter()
                .filter(|e| e.os == os.as_str())
                .map(|e| e.pool.clone())
                .collect();
            (os.as_str(), pools)
        })
        .collect();

    let os = prop_oneof![Just(Os::Linux), Just(Os::Windows), Just(Os::Macos)];
    let native = (
        os.clone(),
        any::<prop::sample::Index>(),
        any::<bool>(),
        any::<bool>(),
        any::<bool>(),
    )
        .prop_map(move |(os, idx, pick, cache_only, package)| {
            let pools = &native_pools[os.as_str()];
            (
                Mode::Native {
                    os,
                    exec_pool: (pick && !pools.is_empty()).then(|| idx.get(pools).clone()),
                    xcode: None,
                },
                cache_only,
                package,
            )
        });
    let cross = (
        any::<prop::sample::Index>(),
        any::<prop::sample::Index>(),
        any::<bool>(),
    )
        .prop_map(move |(ti, pi, pick)| {
            let (name, pools) = ti.get(&targets);
            (
                Mode::Cross {
                    target: name.clone(),
                    exec_pool: pick.then(|| pi.get(pools).clone()),
                },
                false,
                false,
            )
        });
    (
        prop_oneof![native, cross],
        prop_oneof![
            Just("grpcs://cucina.example.com:443".to_string()),
            Just("grpcs://rbe.internal.example".to_string())
        ],
        prop_oneof![Just("main".to_string()), Just("tenant-a".to_string())],
        prop_oneof![
            Just("%workspace%/tools/cucina-credential-helper".to_string()),
            Just("/usr/local/bin/cucina-credential-helper".to_string()),
            Just(r"C:\Program Files\Cucina\cucina-credential-helper.exe".to_string())
        ],
        proptest::option::of(Just("/etc/cucina/ca.pem".to_string())),
        proptest::option::of(prop_oneof![
            Just("cucina".to_string()),
            Just("rbe-ci".to_string())
        ]),
        (any::<bool>(), any::<bool>()),
        proptest::option::of(prop_oneof![
            Just("/home/dev/.cache/bazel-disk-cache".to_string()),
            Just(r"C:\Users\dev\AppData\Local\bazel-disk-cache".to_string())
        ]),
        os,
    )
        .prop_map(
            |(
                (mode, cache_only, package),
                endpoint,
                instance,
                helper,
                ca,
                config,
                (ci, ro),
                disk,
                client_os,
            )| {
                Request {
                    labels: if package {
                        Labels::Package("//tools/cucina".into())
                    } else {
                        Labels::Module
                    },
                    mode,
                    endpoint,
                    instance_name: instance,
                    helper_path: helper,
                    ca_file: ca,
                    config_name: config,
                    ci,
                    read_only: ro,
                    cache_only,
                    disk_cache: disk,
                    disk_cache_max_size: "50G".into(),
                    client_os,
                    header: vec!["generated for a property test".into()],
                }
            },
        )
}

/// Build settings (`--@repo//pkg:name=…`) an exec platform of the catalog requires.
fn catalog_build_settings(cat: &Catalog) -> BTreeSet<String> {
    cat.targets
        .exec_platforms
        .iter()
        .flat_map(|e| e.flags.iter().cloned())
        .collect()
}

fn check_invariants(cat: &Catalog, allow: &Allow, req: &Request, rc: &Bazelrc) {
    // Lines: flags only through `startup` / `build[:config]`, startup first.
    let settings = catalog_build_settings(cat);
    let mut seen_build = false;
    let mut names: Vec<String> = Vec::new();
    for line in &rc.lines {
        let Line::Flag {
            command,
            config,
            flag,
        } = line
        else {
            continue;
        };
        match *command {
            "startup" => {
                assert!(!seen_build, "startup lines come first");
                assert!(config.is_none(), "startup options cannot have a config");
            }
            // `build`, never `common`: common: lines of a config expand before its
            // build: lines and would lose against other rc files (ADR 0807).
            "build" => {
                seen_build = true;
                assert_eq!(config, &req.config_name, "config scoping");
            }
            other => panic!("unexpected command {other}"),
        }
        let name = flag
            .strip_prefix("--")
            .expect("flag")
            .split('=')
            .next()
            .unwrap()
            .to_string();
        let scope = if *command == "startup" {
            "startup"
        } else {
            "build"
        };
        let known = if name.starts_with('@') || name.starts_with("//") {
            // A build setting: only verbatim from the catalog's exec platforms.
            settings.contains(flag.as_str())
        } else {
            allow.contains(&(scope.to_string(), name.clone(), false))
                || allow.contains(&(scope.to_string(), name.clone(), true))
                || name
                    .strip_prefix("no")
                    .is_some_and(|n| allow.contains(&(scope.to_string(), n.to_string(), true)))
        };
        assert!(known, "--{name} is not a Bazel 9.2 {scope} flag");
        names.push(name);
        // The rendered line tokenizes back to exactly [command(:config), flag].
        let rendered = Bazelrc {
            lines: vec![line.clone()],
            notes: vec![],
        }
        .to_string();
        let tokens = tokenize(rendered.trim_end());
        assert_eq!(tokens.len(), 2, "one flag per line: {rendered}");
        assert_eq!(&tokens[1], flag, "quoting survives Bazel's rc tokenizer");
    }
    for n in &names {
        if n != "repo_env" && n != "test_env" {
            assert_eq!(
                names.iter().filter(|m| *m == n).count(),
                1,
                "--{n} emitted once"
            );
        }
    }
    for banned in [
        "remote_header",
        "experimental_remote_cache_chunking",
        "experimental_remote_merkle_tree_cache",
    ] {
        assert!(
            !names.iter().any(|n| n == banned),
            "--{banned} must never be emitted"
        );
    }
    assert_eq!(rc.value("experimental_remote_repo_contents_cache"), None);
    assert!(
        rc.flags()
            .any(|(c, f)| c == "startup" && f == "--experimental_remote_repo_contents_cache")
    );

    // Endpoint and the host-scoped credential helper.
    let host = cucinactl::config::host_of(&req.endpoint).unwrap();
    assert_eq!(
        rc.value("credential_helper"),
        Some(format!("{host}={}", req.helper_path).as_str())
    );
    assert_eq!(rc.value("remote_cache"), Some(req.endpoint.as_str()));
    assert_eq!(
        rc.value("remote_instance_name"),
        Some(req.instance_name.as_str())
    );
    assert_eq!(rc.value("tls_certificate"), req.ca_file.as_deref());
    assert_eq!(
        rc.value("remote_executor"),
        (!req.cache_only).then_some(req.endpoint.as_str())
    );
    assert_eq!(
        rc.value("remote_download_outputs"),
        Some(if req.ci { "minimal" } else { "toplevel" })
    );
    assert_eq!(
        rc.value("remote_upload_local_results"),
        req.read_only.then_some("false"),
        "read-only principals never upload"
    );
    for fixed in [
        "--remote_cache_compression",
        "--rewind_lost_inputs",
        "--nolegacy_important_outputs",
        "--remote_build_event_upload=minimal",
        "--experimental_remote_cache_eviction_retries=5",
        "--remote_retries=10",
        "--remote_retry_max_delay=30s",
        "--grpc_keepalive_time=30s",
    ] {
        assert!(rc.flags().any(|(_, f)| f == fixed), "{fixed} missing");
    }
    if !req.cache_only {
        assert!(rc.flags().any(|(_, f)| f == "--jobs=200"));
        assert!(rc.flags().any(|(_, f)| f == "--noremote_local_fallback"));
    }

    // Settings and lines that only cross configurations or Windows clients get.
    let windows_client_cross =
        matches!(req.mode, Mode::Cross { .. }) && req.client_os == Os::Windows;
    for (flag, value) in [
        ("action_env", "PATH=/bin:/usr/bin:/usr/local/bin"),
        ("host_action_env", "PATH=/bin:/usr/bin:/usr/local/bin"),
    ] {
        assert_eq!(
            rc.value(flag),
            windows_client_cross.then_some(value),
            "--{flag}: Windows clients of cross configurations only"
        );
    }
    assert_eq!(
        rc.flags().any(|f| f == ("build", "--enable_runfiles")),
        windows_client_cross
    );
    assert_eq!(
        rc.flags()
            .any(|f| f == ("startup", "--windows_enable_symlinks")),
        windows_client_cross
    );
    let emitted_settings: Vec<&str> = rc
        .flags()
        .map(|(_, f)| f)
        .filter(|f| f.starts_with("--@") || f.starts_with("--//"))
        .collect();

    // Platforms.
    if req.cache_only {
        assert_eq!(rc.value("platforms"), None);
        assert!(emitted_settings.is_empty() && rc.notes.is_empty());
        return;
    }
    let exec: Vec<&str> = rc
        .value("extra_execution_platforms")
        .expect("exec platforms")
        .split(',')
        .collect();
    let host_platform = rc.value("host_platform").expect("host platform");
    let platforms = rc
        .value("platforms")
        .expect("--platforms (Bazel 9 test toolchain)");
    assert_eq!(
        host_platform, exec[0],
        "host platform = first (chosen) exec platform"
    );
    let repo_env: Vec<&str> = rc
        .flags()
        .filter_map(|(_, f)| f.strip_prefix("--repo_env="))
        .collect();
    let test_env: Vec<&str> = rc
        .flags()
        .filter_map(|(_, f)| f.strip_prefix("--test_env="))
        .collect();
    match &req.mode {
        Mode::Native { os, exec_pool, .. } => {
            assert_eq!(exec.len(), 1);
            assert_eq!(platforms, exec[0], "native: target = exec platform");
            let e = cat
                .targets
                .exec_platforms
                .iter()
                .find(|e| exec_pool.as_deref().is_none_or(|p| e.pool == p) && e.os == os.as_str())
                .unwrap();
            let want = match &req.labels {
                Labels::Module => e.label.clone(),
                Labels::Package(pkg) => format!("{pkg}:{}-{}", e.pool, e.runner),
            };
            if exec_pool.is_some() {
                assert_eq!(exec[0], want);
            }
            assert!(repo_env.is_empty() && test_env.is_empty());
            assert_eq!(rc.value("test_timeout"), None);
            assert!(rc.notes.is_empty());
            // The chosen platform's flags with the module's labels (they name
            // @cucina_platforms settings), none with workspace labels.
            let chosen = cat
                .targets
                .exec_platforms
                .iter()
                .find(|e| exec_label_of(e, &req.labels) == exec[0])
                .expect("emitted exec platform is in the catalog");
            let want_settings: Vec<&str> = match req.labels {
                Labels::Module => chosen.flags.iter().map(String::as_str).collect(),
                Labels::Package(_) => vec![],
            };
            assert_eq!(emitted_settings, want_settings);
        }
        Mode::Cross { target, exec_pool } => {
            let t = cat.target(target).unwrap();
            assert_eq!(platforms, t.platform);
            assert!(
                rc.flags()
                    .any(|(_, f)| f == "--experimental_platform_in_output_dir")
            );
            let allowed: Vec<&str> = t
                .exec_platforms
                .iter()
                .map(|n| cat.exec_platform(n).unwrap().label.as_str())
                .collect();
            let compile_count = allowed.len();
            // Compile exec platforms first (all allowed, each once), then the test twin.
            let compile: BTreeSet<&str> = exec[..compile_count].iter().copied().collect();
            assert_eq!(
                compile,
                allowed.iter().copied().collect(),
                "compile exec platforms first"
            );
            match &t.test {
                Some(test) => {
                    assert_eq!(exec.len(), compile_count + 1);
                    assert_eq!(exec[compile_count], test.label, "test exec platform last");
                }
                None => assert_eq!(
                    exec.len(),
                    compile_count,
                    "build-only target has no test platform"
                ),
            }
            if let Some(pool) = exec_pool {
                let chosen = cat
                    .targets
                    .exec_platforms
                    .iter()
                    .find(|e| &e.pool == pool)
                    .unwrap();
                assert_eq!(
                    exec[0], chosen.label,
                    "--exec-pool decides the first exec platform"
                );
            } else {
                assert_eq!(exec[0], allowed[0], "default order");
            }
            if t.os == "macos" {
                let first = cat
                    .targets
                    .exec_platforms
                    .iter()
                    .find(|e| e.label == exec[0])
                    .unwrap();
                assert_eq!(first.os, "macos", "macOS targets compile on macOS");
            }
            let eula = [
                "BAZEL_MSVC_RUNTIME_VISUAL_STUDIO_EULA=1",
                "BAZEL_WINDOWS_SDK_EULA=1",
            ];
            if t.is_msvc() {
                assert_eq!(repo_env, eula, "MSVC targets accept the EULAs");
            } else {
                assert!(repo_env.is_empty(), "only MSVC targets get EULA flags");
            }
            if t.os == "windows" {
                let keys: Vec<&str> = test_env
                    .iter()
                    .map(|e| e.split('=').next().unwrap())
                    .collect();
                assert_eq!(
                    keys,
                    ["SYSTEMROOT", "PATH"],
                    "Windows test env from every client OS"
                );
                assert!(test_env[0].ends_with(r"=C:\Windows"));
            } else {
                assert!(test_env.is_empty());
            }
            // The @bazel_tools overlay note: Windows tests from Linux/macOS clients.
            assert_eq!(
                rc.notes.len(),
                usize::from(t.os == "windows" && req.client_os != Os::Windows)
            );
            // Emulated runners: Bazel's timeouts scaled by testTimeoutScale.
            match (t.test_timeout_scale, &t.test) {
                (Some(k), Some(_)) if k != 1.0 => {
                    let want: Vec<String> = [60.0, 300.0, 900.0, 3600.0]
                        .iter()
                        .map(|b: &f64| ((b * k).round() as u64).to_string())
                        .collect();
                    assert_eq!(rc.value("test_timeout"), Some(want.join(",").as_str()));
                }
                _ => assert_eq!(rc.value("test_timeout"), None),
            }
            // Exec-platform flags: those of the listed compile platforms, chosen
            // first, the first of each name winning.
            let mut want_settings: Vec<&str> = Vec::new();
            let mut names: Vec<&str> = Vec::new();
            for label in &exec[..compile_count] {
                let e = cat
                    .targets
                    .exec_platforms
                    .iter()
                    .find(|e| e.label == *label)
                    .unwrap();
                for f in &e.flags {
                    let n = f.split('=').next().unwrap();
                    if !names.contains(&n) {
                        names.push(n);
                        want_settings.push(f);
                    }
                }
            }
            assert_eq!(emitted_settings, want_settings);
        }
    }
}

fn exec_label_of(e: &cucinactl::catalog::ExecPlatform, labels: &Labels) -> String {
    match labels {
        Labels::Module => e.label.clone(),
        Labels::Package(pkg) => format!("{pkg}:{}-{}", e.pool, e.runner),
    }
}

proptest! {
    #![proptest_config(ProptestConfig { cases: 256, failure_persistence: None, ..ProptestConfig::default() })]

    // UC9 / R-DATA-1 / R-XPLAT-2(d) / R-AUTH-8: invariants of every generated .bazelrc.
    #[test]
    fn generated_bazelrc_respects_invariants(req in request_strategy()) {
        let cat = catalog();
        let allow = allowlist();
        match bazelrc::generate(cat, &req) {
            Ok(rc) => check_invariants(cat, allow, &req, &rc),
            // The only legitimate refusal: workspace-package labels for a cross target.
            Err(e) => prop_assert!(
                matches!(req.labels, Labels::Package(_)) && matches!(req.mode, Mode::Cross { .. }),
                "unexpected error: {e:#}"
            ),
        }
    }
}

// R-XPLAT-6/-1: unsupported placements are refused with a usage error.
#[test]
fn unsupported_placements_are_refused() {
    let cat = catalog();
    let base = Request {
        mode: Mode::Cross {
            target: "aarch64-apple-darwin".into(),
            exec_pool: Some("linux-x86-64".into()),
        },
        endpoint: "grpcs://cucina.example.com".into(),
        instance_name: "main".into(),
        helper_path: "cucina-credential-helper".into(),
        ca_file: None,
        config_name: None,
        ci: false,
        read_only: false,
        cache_only: false,
        disk_cache: None,
        disk_cache_max_size: "50G".into(),
        client_os: Os::Linux,
        labels: Labels::Module,
        header: vec![],
    };
    let cases = [
        ("macOS target compiled on Linux", base.clone()),
        (
            "excluded target",
            Request {
                mode: Mode::Cross {
                    target: "x86_64-apple-darwin".into(),
                    exec_pool: None,
                },
                ..base.clone()
            },
        ),
        (
            "unknown target",
            Request {
                mode: Mode::Cross {
                    target: "sparc64-sun-solaris".into(),
                    exec_pool: None,
                },
                ..base.clone()
            },
        ),
        (
            "cross configuration without remote execution",
            Request {
                mode: Mode::Cross {
                    target: "x86_64-linux-gnu".into(),
                    exec_pool: None,
                },
                cache_only: true,
                ..base.clone()
            },
        ),
        (
            "plaintext endpoint",
            Request {
                mode: Mode::Native {
                    os: Os::Linux,
                    exec_pool: None,
                    xcode: None,
                },
                endpoint: "grpc://cucina.example.com".into(),
                ..base.clone()
            },
        ),
    ];
    for (name, req) in cases {
        let err = bazelrc::generate(cat, &req).expect_err(name);
        let code = cucinactl::exit::exit_code_for(&err);
        assert!(
            matches!(
                code,
                cucinactl::exit::ExitCode::Usage | cucinactl::exit::ExitCode::NotFound
            ),
            "{name}: {code:?}"
        );
    }
}
