from __future__ import annotations

import importlib
import subprocess
from datetime import datetime

import pytest

from e2e.clients.lsp import LspApiClient
from e2e.clients.rln import RlnClient
from e2e.support.config import E2EConfig
from e2e.support.harness import (
    Env,
    create_utxos,
    docker_unlock_payload,
    dump_current_state,
    ensure_faucet_asset_balance,
    ensure_rln_docker_image,
    fund_nodes,
    remove_container,
    reset_environment,
    seed_lsp_from_faucet,
    spawn_rln_node,
    spawn_utexo_lsp,
    terminate_process,
    wait_for_peer_channel_usable,
    wait_for_channels_usable,
    wait_for_rln_boot,
    wait_for_utexo_boot,
)


@pytest.fixture(scope="session")
def cfg():
    return E2EConfig()


def ensure_sdk_available():
    try:
        importlib.import_module("rgb_lightning_node")
    except ImportError:
        rgbln_repo = "${RGBLN_REPO:-/path/to/rgb-lightning-node}"
        pytest.exit(
            "rgb_lightning_node not found. Build it first:\n"
            f"  cd {rgbln_repo}\n"
            "  cargo build --release --features uniffi --lib\n"
            "  ./scripts/ci/uniffi_generate_python.sh\n"
            f"  export PYTHONPATH={rgbln_repo}/target/uniffi/python:${{PYTHONPATH:-}}\n"
            f"  export LD_LIBRARY_PATH={rgbln_repo}/target/release:${{LD_LIBRARY_PATH:-}}",
            returncode=1,
        )


@pytest.fixture(scope="session", autouse=True)
def check_sdk_available():
    ensure_sdk_available()


@pytest.fixture(scope="session", autouse=True)
def check_docker_available():
    try:
        subprocess.run(["docker", "version"], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    except (FileNotFoundError, subprocess.CalledProcessError):
        pytest.exit(
            "docker is required for the E2E harness because LSP RLN and Faucet run as dockerized daemons.",
            returncode=1,
        )


@pytest.fixture(scope="session", autouse=True)
def check_regtest_available(cfg: E2EConfig):
    # Regtest lifecycle stays outside pytest on purpose. This matches the upstream
    # rgb-lightning-node workflow and avoids interactive `sudo` cleanup paths in
    # regtest.sh start/stop during automated runs.
    try:
        output = subprocess.run(
            ["docker", "compose", "ps", "--services", "--status", "running"],
            cwd=cfg.rgbln_repo,
            check=True,
            capture_output=True,
            text=True,
        )
    except (FileNotFoundError, subprocess.CalledProcessError) as exc:
        pytest.exit(
            "could not query regtest services; start them first with './regtest.sh start' in rgb-lightning-node",
            returncode=1,
        )

    services = {line.strip() for line in output.stdout.splitlines() if line.strip()}
    missing = [service for service in ("bitcoind", "electrs", "proxy") if service not in services]
    if missing:
        pytest.exit(
            f"regtest services not running: {', '.join(missing)}; start them first with './regtest.sh start' in rgb-lightning-node",
            returncode=1,
        )


@pytest.fixture(scope="session", autouse=True)
def ensure_docker_image(cfg: E2EConfig):
    ensure_rln_docker_image(cfg)


@pytest.fixture(scope="function")
def env(cfg: E2EConfig, request: pytest.FixtureRequest):
    reset_environment(cfg)

    artifact_dir = cfg.logs_dir / datetime.now().strftime("%Y%m%d-%H%M%S-%f")
    artifact_dir.mkdir(parents=True, exist_ok=True)
    docker_datadirs = {
        "lsp": artifact_dir / "docker-lsp",
        "faucet": artifact_dir / "docker-faucet",
        "user_a": artifact_dir / "docker-user-a",
        "user_b": artifact_dir / "docker-user-b",
    }

    node_logs = {
        "lsp": artifact_dir / "rln-lsp.log",
        "faucet": artifact_dir / "rln-faucet.log",
        "user_a": artifact_dir / "rln-user-a.log",
        "user_b": artifact_dir / "rln-user-b.log",
    }
    node_processes = {
        "lsp": spawn_rln_node(
            cfg,
            cfg.lsp_container_name,
            docker_datadirs["lsp"],
            cfg.lsp_daemon_port,
            cfg.lsp_peer_port,
            node_logs["lsp"],
        ),
        "faucet": spawn_rln_node(
            cfg,
            cfg.faucet_container_name,
            docker_datadirs["faucet"],
            cfg.faucet_daemon_port,
            cfg.faucet_peer_port,
            node_logs["faucet"],
        ),
        "user_a": spawn_rln_node(
            cfg,
            cfg.user_a_container_name,
            docker_datadirs["user_a"],
            cfg.user_a_daemon_port,
            cfg.user_a_peer_port,
            node_logs["user_a"],
        ),
        "user_b": spawn_rln_node(
            cfg,
            cfg.user_b_container_name,
            docker_datadirs["user_b"],
            cfg.user_b_daemon_port,
            cfg.user_b_peer_port,
            node_logs["user_b"],
        ),
    }
    for name, process in (
        (cfg.lsp_container_name, node_processes["lsp"]),
        (cfg.faucet_container_name, node_processes["faucet"]),
        (cfg.user_a_container_name, node_processes["user_a"]),
        (cfg.user_b_container_name, node_processes["user_b"]),
    ):
        request.addfinalizer(lambda name=name: remove_container(name))
        request.addfinalizer(lambda process=process: terminate_process(process))

    clients = {
        "lsp": RlnClient(cfg.lsp_url),
        "user_a": RlnClient(cfg.user_a_url),
        "user_b": RlnClient(cfg.user_b_url),
        "faucet": RlnClient(cfg.faucet_url),
    }

    wait_for_rln_boot(clients["lsp"], cfg)
    wait_for_rln_boot(clients["faucet"], cfg)
    wait_for_rln_boot(clients["user_a"], cfg)
    wait_for_rln_boot(clients["user_b"], cfg)

    clients["lsp"].init(cfg.password)
    clients["faucet"].init(cfg.password)
    clients["user_a"].init(cfg.password)
    clients["user_b"].init(cfg.password)
    clients["lsp"].unlock_with_payload(docker_unlock_payload(cfg))
    clients["faucet"].unlock_with_payload(docker_unlock_payload(cfg))
    clients["user_a"].unlock_with_payload(docker_unlock_payload(cfg))
    clients["user_b"].unlock_with_payload(docker_unlock_payload(cfg))

    status, _ = clients["lsp"].post_allow_error("/sendrgb", {})
    if status == 404:
        raise AssertionError("RLN /sendrgb endpoint returned 404; rebuild rgb-lightning-node with cargo install --locked --path . --force")

    fund_nodes(cfg, clients)
    create_utxos(cfg, clients)

    asset_resp = clients["faucet"].issueassetnia()
    asset_id = asset_resp["asset"]["asset_id"]
    ensure_faucet_asset_balance(cfg, clients["faucet"], asset_id)
    seed_lsp_from_faucet(cfg, clients["lsp"], clients["faucet"], asset_id)

    utexo_log = artifact_dir / "utexo-lsp.log"
    utexo_proc = spawn_utexo_lsp(cfg, asset_id, utexo_log)
    request.addfinalizer(lambda: terminate_process(utexo_proc))

    lsp_api = LspApiClient(cfg.utexo_url)
    wait_for_utexo_boot(lsp_api, cfg)

    lsp_pubkey = lsp_api.get_info()["pubkey"]

    env_obj = Env(
        cfg=cfg,
        lsp_rln=clients["lsp"],
        user_a=clients["user_a"],
        user_b=clients["user_b"],
        faucet=clients["faucet"],
        lsp_api=lsp_api,
        asset_id=asset_id,
        lsp_pubkey=lsp_pubkey,
        artifact_dir=artifact_dir,
    )
    request.addfinalizer(lambda: dump_current_state(env_obj))

    # Deterministic bring-up: avoid concurrent LSP channel opens racing on RGB allocations.
    clients["user_a"].connectpeer(f"{lsp_pubkey}@{cfg.lsp_container_name}:{cfg.lsp_peer_port}")
    wait_for_peer_channel_usable(env_obj, clients["user_a"], label="user_a")
    clients["user_b"].connectpeer(f"{lsp_pubkey}@{cfg.lsp_container_name}:{cfg.lsp_peer_port}")
    wait_for_peer_channel_usable(env_obj, clients["user_b"], label="user_b")

    wait_for_channels_usable(env_obj)

    return env_obj
