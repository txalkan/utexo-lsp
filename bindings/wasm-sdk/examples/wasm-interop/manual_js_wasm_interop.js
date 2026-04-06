import init, {
  rgbGenerateKeysJson,
  rgbGenerateKeysValue,
  rgbRestoreKeysJson,
  RlnWasmInvoice,
  checkProxyUrl,
} from "../../pkg/rln_wasm_sdk.js";

function log(message, data = undefined) {
  const out = document.getElementById("out");
  if (!out) return;
  const line = document.createElement("pre");
  line.textContent = data === undefined ? String(message) : `${message}: ${JSON.stringify(data, null, 2)}`;
  out.appendChild(line);
}

async function run() {
  await init();
  log("WASM initialized");

  const keysJson = rgbGenerateKeysJson("regtest");
  const keysFromJson = JSON.parse(keysJson);
  const keysFromValue = rgbGenerateKeysValue("regtest");
  log("Generated keys (json)", keysFromJson);

  if (keysFromJson.xpub !== keysFromValue.xpub) {
    throw new Error("xpub mismatch between json/value key generation");
  }
  log("Key generation parity", { ok: true });

  const restoredJson = rgbRestoreKeysJson("regtest", keysFromJson.mnemonic);
  const restored = JSON.parse(restoredJson);
  log("Restored keys", restored);

  if (restored.xpub !== keysFromJson.xpub) {
    throw new Error("xpub mismatch between generated and restored keys");
  }
  log("Restore parity", { ok: true });

  try {
    // This is expected to fail; demonstrates stable error contract.
    new RlnWasmInvoice("");
  } catch (err) {
    log("Invoice parse expected error", String(err));
  }

  const proxyInput = document.getElementById("proxyUrl");
  const proxyUrl = proxyInput && proxyInput.value ? proxyInput.value.trim() : "";
  if (proxyUrl.length > 0) {
    try {
      await checkProxyUrl(proxyUrl);
      log("Proxy check", { ok: true, proxyUrl });
    } catch (err) {
      log("Proxy check failed", String(err));
    }
  } else {
    log("Proxy check skipped", "Set proxy URL in the input field to test checkProxyUrl");
  }

  log("Example finished", { ok: true });
}

const runBtn = document.getElementById("run");
if (runBtn) {
  runBtn.addEventListener("click", () => {
    run().catch((err) => log("Fatal error", String(err)));
  });
}
