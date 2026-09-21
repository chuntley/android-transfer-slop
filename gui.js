"use strict";

(() => {
  const byId = (id) => document.getElementById(id);
  const token = document.querySelector('meta[name="transfer-token"]').content;
  const ui = Object.fromEntries([
    "settings", "device", "device-help", "device-error", "refresh-devices",
    "phone-path", "choose-source", "source-browser", "up-folder", "browse-status",
    "browse-error", "folder-list", "folder-pagination", "previous-folders",
    "next-folders", "folder-page", "folder-breadcrumbs", "folder-row", "sources-error",
    "destination", "choose-destination", "destination-error", "advanced",
    "batch-size", "batch-bytes", "max-batches", "form-error", "start", "verify", "safe-delete", "quick-delete",
    "stop", "run-panel", "run-status", "progress", "progress-counts", "current-file",
    "run-error", "stop-error", "activity-log", "connection-error",
    "verification-counts", "verification-help", "download-report", "report-error",
  ].map((id) => [id, byId(id)]));
  const pageSize = 100;
  let devices = [];
  let selectedSerial = "";
  let folders = [];
  let folderPage = 0;
  let browsedPath = "";
  let browseSequence = 0;
  let browseController = null;
  let browsing = false;
  let refreshing = false;
  let busy = false;
  let stopping = false;
  let statusKnown = false;
  let disconnected = false;
  let runState = "idle";
  let restoredRunId = 0;
  let statusEpoch = 0;
  let polling = false;
  let pollTimer = null;
  const activity = ui["activity-log"].closest("details");
  let latestLog = "No activity yet.";

  function setDisabled(element, disabled) {
    disabled = Boolean(disabled);
    if (element.disabled !== disabled) element.disabled = disabled;
  }

  function setHidden(element, hidden) {
    if (element.hidden !== hidden) element.hidden = hidden;
  }

  function renderLog() {
    if (!activity.open || ui["activity-log"].textContent === latestLog) return;
    const log = ui["activity-log"];
    const follow = log.scrollHeight - log.scrollTop - log.clientHeight < 32;
    setText(log, latestLog);
    if (follow) requestAnimationFrame(() => { log.scrollTop = log.scrollHeight; });
  }

  function setText(element, text) {
    if (element.textContent !== text) element.textContent = text;
  }

  function message(id, text) {
    setText(ui[id], text);
    setHidden(ui[id], !text);
  }

  async function request(path, data, options = {}) {
    const controller = options.controller || new AbortController();
    const timeout = options.timeout === undefined ? 30000 : options.timeout;
    const timer = timeout ? setTimeout(() => controller.abort(), timeout) : null;
    try {
      const response = await fetch(path, {
        method: options.method || (data === undefined ? "GET" : "POST"),
        headers: { "X-Transfer-Token": token, ...(data === undefined ? {} : { "Content-Type": "application/json" }) },
        body: data === undefined ? undefined : JSON.stringify(data),
        cache: "no-store",
        credentials: "same-origin",
        signal: controller.signal,
      });
      if (response.ok && options.download) {
        const disposition = response.headers.get("Content-Disposition") || "";
        const filename = /filename="([^"]+)"/.exec(disposition)?.[1] || "android-transfer-slop-report.txt";
        return { blob: await response.blob(), filename };
      }
      const result = await response.json();
      if (!response.ok) throw new Error(result.error || `Request failed (${response.status}).`);
      return result;
    } catch (error) {
      if (error.name === "AbortError") throw new Error("The request timed out. Check the phone connection and try again.");
      if (error instanceof TypeError) throw new Error("Cannot reach the local server. Check that Android Transfer SLOP is still running.");
      throw error;
    } finally {
      clearTimeout(timer);
    }
  }

  function activeRun() {
    return runState === "running" || runState === "stopping";
  }

  function readyDevice() {
    return devices.some((device) => device.serial === selectedSerial && device.state === "device");
  }

  function updateControls() {
    const browseBusy = String(browsing);
    if (ui["folder-list"].getAttribute("aria-busy") !== browseBusy) ui["folder-list"].setAttribute("aria-busy", browseBusy);
    setDisabled(ui.settings, activeRun() || busy || !statusKnown || disconnected);
    setDisabled(ui.device, refreshing);
    setDisabled(ui["refresh-devices"], refreshing);
    setText(ui["refresh-devices"], refreshing ? "Refreshing…" : "Refresh");
    setDisabled(ui["start"], refreshing || !readyDevice());
    setDisabled(ui["verify"], refreshing || !readyDevice());
    setDisabled(ui["safe-delete"], refreshing || !readyDevice());
    setDisabled(ui["quick-delete"], refreshing || !readyDevice());
    setDisabled(ui["phone-path"], !readyDevice());
    setDisabled(ui["choose-source"], browsing || !readyDevice());
    setDisabled(ui["up-folder"], browsing || !readyDevice() || !browsedPath || browsedPath === "/");
    setDisabled(ui["previous-folders"], browsing || folderPage === 0);
    setDisabled(ui["next-folders"], browsing || (folderPage + 1) * pageSize >= folders.length);
    const foldersDisabled = browsing || !readyDevice();
    for (const button of ui["source-browser"].querySelectorAll(".folder-list button, .folder-breadcrumbs button")) setDisabled(button, foldersDisabled);
    setHidden(ui.stop, !activeRun());
    setDisabled(ui.stop, stopping || runState === "stopping" || disconnected);
    setText(ui.stop, stopping || runState === "stopping" ? "Stopping…" : "Stop safely");
  }

  function renderBreadcrumbs() {
    const path = browsedPath || ui["phone-path"].value;
    const parts = path.startsWith("/") ? ["/", ...path.split("/").filter(Boolean)] : [];
    const fragment = document.createDocumentFragment();
    let parent = "";
    parts.forEach((name, index) => {
      parent = index === 0 ? "/" : `${parent === "/" ? "" : parent}/${name}`;
      const target = parent;
      const item = document.createElement("li");
      const current = index === parts.length - 1;
      const crumb = document.createElement(current ? "span" : "button");
      crumb.textContent = index === 0 ? "Root" : name;
      crumb.title = target;
      if (current) {
        crumb.setAttribute("aria-current", "location");
        crumb.tabIndex = -1;
      } else {
        crumb.type = "button";
        crumb.setAttribute("aria-label", `Open folder ${target}`);
        crumb.addEventListener("click", () => browse(target, true));
      }
      item.append(crumb);
      fragment.append(item);
    });
    ui["folder-breadcrumbs"].replaceChildren(fragment);
  }

  function renderFolders() {
    renderBreadcrumbs();
    const fragment = document.createDocumentFragment();
    const start = folderPage * pageSize;
    for (const path of folders.slice(start, start + pageSize)) {
      const item = ui["folder-row"].content.firstElementChild.cloneNode(true);
      const open = item.querySelector("button");
      open.querySelector(".folder-name").textContent = path.slice(path.lastIndexOf("/") + 1) || path;
      open.title = path;
      open.setAttribute("aria-label", `Open folder ${path}`);
      open.addEventListener("click", () => browse(path, true));
      fragment.append(item);
    }
    ui["folder-list"].replaceChildren(fragment);
    setHidden(ui["folder-pagination"], folders.length <= pageSize);
    setText(ui["folder-page"], `${start + 1}–${Math.min(start + pageSize, folders.length)} of ${folders.length}`);
    updateControls();
  }

  function deviceGuidance(device) {
    if (!device) return "No devices found. Connect a USB data cable, enable USB debugging, then select Refresh.";
    if (device.state === "unauthorized") return "Unlock your phone and allow this computer’s USB debugging request, then select Refresh.";
    if (device.state !== "device") return `Device is ${device.state}. Reconnect the cable, unlock the phone, then select Refresh.`;
    return "Connected. Transfer and verification keep originals. Source deletion is always a separate, confirmed action.";
  }

  function selectDevice() {
    const nextSerial = ui.device.value;
    if (nextSerial !== selectedSerial) {
      selectedSerial = nextSerial;
      browseSequence++;
      browseController?.abort();
      browsing = false;
      browsedPath = "";
      folders = [];
      folderPage = 0;
      ui["phone-path"].value = "";
      ui["phone-path"].removeAttribute("aria-invalid");
      setHidden(ui["source-browser"], true);
      ui["choose-source"].setAttribute("aria-expanded", "false");
      message("browse-error", "");
      message("sources-error", "");
      renderFolders();
    }
    const device = devices.find((entry) => entry.serial === selectedSerial);
    setText(ui["device-help"], !device && selectedSerial ? "The selected device is not connected. Reconnect it or choose another device." : deviceGuidance(device));
    setText(ui["browse-status"], readyDevice() ? "Open a folder to browse its subfolders." : "Select a connected, authorized device to browse folders.");
    updateControls();
  }

  function renderDevices() {
    const preferred = selectedSerial || (devices.find((device) => device.state === "device") || devices[0])?.serial || "";
    const fragment = document.createDocumentFragment();
    for (const device of devices) {
      fragment.append(new Option(`${device.model ? `${device.model} — ` : ""}${device.serial} (${device.state})`, device.serial));
    }
    if (preferred && !devices.some((device) => device.serial === preferred)) {
      fragment.append(new Option(`${preferred} (not connected)`, preferred));
    } else if (!devices.length) {
      fragment.append(new Option("No connected devices", ""));
    }
    ui.device.replaceChildren(fragment);
    ui.device.value = preferred;
    selectDevice();
  }

  function restoreSettings(status) {
    if (!status.settings || status.runId === restoredRunId) return;
    restoredRunId = status.runId;
    const settings = status.settings;
    browseSequence++;
    browseController?.abort();
    browsing = false;
    browsedPath = "";
    folders = [];
    folderPage = 0;
    selectedSerial = settings.serial;
    ui["phone-path"].value = settings.sources[0] || "";
    ui["phone-path"].removeAttribute("aria-invalid");
    setHidden(ui["source-browser"], true);
    ui["choose-source"].setAttribute("aria-expanded", "false");
    ui.destination.value = settings.dest;
    ui["batch-size"].value = settings.batchSize;
    ui["batch-bytes"].value = settings.batchBytes;
    ui["max-batches"].value = settings.maxBatches;
    renderFolders();
    renderDevices();
  }

  async function refreshDevices() {
    if (refreshing) return;
    refreshing = true;
    message("device-error", "");
    updateControls();
    try {
      const result = await request("/api/devices");
      devices = result.devices || [];
      renderDevices();
    } catch (error) {
      devices = [];
      renderDevices();
      message("device-error", `${error.message} Check that Android SDK Platform-Tools (adb) is installed and available, then select Refresh.`);
    } finally {
      refreshing = false;
      updateControls();
    }
  }

  async function browse(path, focusPath = false) {
    if (!readyDevice()) return;
    setHidden(ui["source-browser"], false);
    ui["choose-source"].setAttribute("aria-expanded", "true");
    if (!path.startsWith("/")) {
      message("browse-error", "Enter an absolute phone folder path, such as /sdcard/DCIM.");
      ui["phone-path"].focus();
      return;
    }
    const sequence = ++browseSequence;
    const serial = selectedSerial;
    browseController?.abort();
    const controller = new AbortController();
    browseController = controller;
    browsing = true;
    browsedPath = "";
    folders = [];
    folderPage = 0;
    ui["phone-path"].value = path;
    ui["phone-path"].removeAttribute("aria-invalid");
    message("sources-error", "");
    message("browse-error", "");
    setText(ui["browse-status"], "Loading folders…");
    renderFolders();
    try {
      const result = await request("/api/browse", { serial, path }, { controller });
      if (sequence !== browseSequence || serial !== selectedSerial) return;
      browsedPath = result.path;
      folders = result.folders || [];
      setText(ui["browse-status"], folders.length ? `${folders.length} ${folders.length === 1 ? "folder" : "folders"}. Open a folder to select it.` : "No subfolders. This folder is ready to transfer.");
      renderFolders();
      if (focusPath) ui["folder-breadcrumbs"].querySelector("[aria-current]")?.focus();
    } catch (error) {
      if (sequence !== browseSequence || serial !== selectedSerial) return;
      message("browse-error", error.message);
      setText(ui["browse-status"], "Folder could not be opened. Check the path and phone connection, then select Choose folder.");
    } finally {
      if (sequence === browseSequence) {
        browsing = false;
        updateControls();
      }
    }
  }

  function renderStatus(status) {
    runState = status.state;
    restoreSettings(status);
    const progress = status.progress || {};
    const total = progress.total || 0;
    const verified = progress.verified || 0;
    const copied = progress.copied || 0;
    const verifying = Boolean(status.settings?.verify);
    const safeDeleting = Boolean(status.settings?.safeDelete);
    const quickDeleting = Boolean(status.settings?.quickDelete);
    const deleting = safeDeleting || quickDeleting;
    const deleted = progress.deleted || 0;
    const checked = progress.checked || 0;
    const processed = verifying || deleting ? checked : verified;
    const scanning = progress.phase === "scanning";
    const scanned = progress.scanned || 0;
    const labels = {
      idle: "Ready when you are. Select a source folder, then start a transfer.",
      running: scanning ? "Scanning phone files. The total is not known yet." : progress.phase ? `Working: ${progress.phase}.` : "Transfer in progress…",
      stopping: "Stopping safely. Completed copies will be kept.",
      completed: verified < total ? "Run completed. Some files remain; start again to continue." : "Run completed. All processed files are verified.",
      stopped: "Transfer stopped. Completed copies are kept. Start again to resume.",
      failed: "Transfer failed. Completed copies are kept. Resolve the error below, then try again.",
    };
    if (verifying) {
      labels.completed = "Verification completed. All checked files match.";
      labels.stopped = "Verification stopped. The report covers files checked so far.";
      labels.failed = !scanning && checked === total
        ? "Verification finished with problems. Review the counts and report below."
        : "Verification could not finish. Review the error and partial report below.";
    }
    if (deleting) {
      const mode = quickDeleting ? "Quick Source Delete" : "Safe Source Delete";
      labels.running = scanning
        ? `Scanning phone files before ${quickDeleting ? "quick deletion" : "deletion"}…`
        : quickDeleting
          ? "Checking file metadata and deleting matching source files…"
          : "Verifying copies and deleting matching source files…";
      labels.stopping = "Stopping source deletion. Files already deleted cannot be restored by this app.";
      labels.completed = `${mode} completed. All selected files were checked and deleted from the phone.`;
      labels.stopped = "Source deletion stopped. Review the report before running again.";
      labels.failed = !scanning && checked === total
        ? "Source deletion finished with issues. Review the counts and report."
        : "Source deletion could not finish. Review the error and partial report.";
    }
    if (ui["run-panel"].dataset.state !== runState) ui["run-panel"].dataset.state = runState;
    setText(ui["run-status"], labels[runState] || `Transfer state: ${runState}`);
    if (ui.progress.max !== (total || 1)) ui.progress.max = total || 1;
    if (activeRun() && (scanning || total === 0)) {
      if (ui.progress.hasAttribute("value")) ui.progress.removeAttribute("value");
    } else if (!ui.progress.hasAttribute("value") || ui.progress.value !== processed) {
      ui.progress.value = processed;
    }
    ui.progress.setAttribute("aria-label", scanning ? "Scanning phone files" : verifying || deleting ? "Files checked" : "Files verified");
    setText(ui["progress-counts"], scanning
      ? `${scanned.toLocaleString()} ${scanned === 1 ? "file" : "files"} found${activeRun() ? " · still scanning" : ""}`
      : deleting ? `${checked} of ${total} checked · ${deleted} deleted · ${Math.max(0, checked - deleted)} retained / unconfirmed · ${Math.max(0, total - checked)} not checked`
      : verifying ? `${checked} of ${total} checked · ${verified} matched · ${Math.max(0, total - checked)} not checked`
        : `${copied} copied · ${verified}${total ? ` of ${total}` : ""} verified`);
    message("current-file", progress.current ? `${scanning ? "Latest file found" : "Current file"}: ${progress.current}` : "");
    message("run-error", status.error || "");
    setHidden(ui["verification-counts"], !verifying && !deleting);
    setHidden(ui["verification-help"], !verifying && !deleting);
    setText(ui["verification-help"], deleting
      ? quickDeleting
        ? "Quick Source Delete checks path, size, and modification time only; it does not hash contents. Same-size rewrites or restored timestamps can evade this check. Missing, mismatched, changed, or unverifiable files are retained. Destination copies are not changed. Keep both folders idle and a second backup."
        : "Safe Source Delete requires a fresh SHA-256 match. Missing, mismatched, changed, or unverifiable files are retained. A failed or interrupted delete may have reached the phone; check the source before rerunning. Destination copies are not changed. Keep both folders idle and a second backup."
      : "Transfer mode repairs missing or mismatched copies using the phone’s version. Changed sources need a fresh check. Download the report for details; a download during a run is a partial report.");
    setText(ui["download-report"], deleting ? "Download deletion report" : "Download verification report");
    setText(ui["verification-counts"], `${progress.missing || 0} missing · ${progress.mismatched || 0} mismatched · ${progress.changed || 0} source changed · ${progress.errors || 0} errors`);
    setHidden(ui["download-report"], !status.reportAvailable);
    latestLog = (status.logs || []).slice(-100).join("\n") || "No activity yet.";
    if (latestLog.length > 16384) latestLog = "[Older activity omitted from view]\n" + latestLog.slice(-16384);
    renderLog();
    updateControls();
  }

  async function pollStatus() {
    clearTimeout(pollTimer);
    if (polling || document.hidden) return;
    polling = true;
    const epoch = statusEpoch;
    try {
      const status = await request("/api/status", undefined, { timeout: 10000 });
      if (epoch !== statusEpoch) return;
      statusKnown = true;
      disconnected = false;
      message("connection-error", "");
      renderStatus(status);
    } catch (error) {
      if (epoch !== statusEpoch) return;
      disconnected = true;
      message("connection-error", `${error.message} Transfer status is unknown; the transfer may still be running. This page will reconnect automatically.`);
      updateControls();
    } finally {
      polling = false;
      if (!document.hidden) pollTimer = setTimeout(pollStatus, 1000);
    }
  }

  async function startTransfer(verify, safeDelete = false, quickDelete = false) {
    if (activeRun() || busy || refreshing || disconnected || !statusKnown) return;
    for (const id of ["form-error", "sources-error", "destination-error", "stop-error"]) message(id, "");
    ui.destination.removeAttribute("aria-invalid");
    ui["phone-path"].removeAttribute("aria-invalid");
    if (!readyDevice()) {
      message("form-error", "Select a connected, authorized phone before starting.");
      ui.device.focus();
      return;
    }
    const source = ui["phone-path"].value;
    if (!source.trim()) {
      message("sources-error", "Choose or enter a source folder before starting.");
      ui["phone-path"].setAttribute("aria-invalid", "true");
      ui["phone-path"].focus();
      return;
    }
    if (!ui.destination.value.trim()) {
      message("destination-error", "Choose or enter a destination folder before starting.");
      ui.destination.setAttribute("aria-invalid", "true");
      ui.destination.focus();
      return;
    }
    const values = {};
    for (const [id, key, minimum] of [["batch-size", "batchSize", 1], ["batch-bytes", "batchBytes", 1], ["max-batches", "maxBatches", 0]]) {
      const value = ui[id].valueAsNumber;
      if (!Number.isSafeInteger(value) || value < minimum) {
        ui.advanced.open = true;
        message("form-error", `${ui[id].labels[0].textContent} must be a whole number of at least ${minimum}, within the supported numeric range.`);
        ui[id].focus();
        return;
      }
      values[key] = value;
    }
    const settings = { serial: selectedSerial, sources: [source], dest: ui.destination.value, ...values, verify, safeDelete, quickDelete };
    if (safeDelete && !window.confirm(
      `Permanently delete source files after SHA-256 verification?\n\nPhone: ${settings.serial}\nSource: ${source}\nDestination: ${settings.dest}\n\nOnly files with freshly matching SHA-256 content will be deleted. Missing or mismatched copies are retained. All source files are checked; batch limits do not apply.\n\nKeep both folders idle and keep a second backup. Deletion cannot be undone by this app.`
    )) return;
    if (quickDelete && !window.confirm(
      `Quickly delete source files using basic metadata only?\n\nPhone: ${settings.serial}\nSource: ${source}\nDestination: ${settings.dest}\n\nThis checks path, size, and modification time but does NOT hash file contents. Same-size rewrites or restored timestamps can be deleted. Missing or mismatched files are retained. Keep both folders idle and keep a second backup.\n\nDeletion cannot be undone by this app.`
    )) return;
    busy = true;
    updateControls();
    let attemptedStart = false;
    try {
      const current = await request("/api/devices");
      if (!(current.devices || []).some((device) => device.serial === selectedSerial && device.state === "device")) {
        throw new Error("The selected phone is no longer connected and authorized. Reconnect it, allow USB debugging, then select Refresh.");
      }
      statusEpoch++;
      attemptedStart = true;
      await request("/api/start", settings);
      runState = "running";
      setText(ui["run-status"], quickDelete ? "Starting quick source deletion…" : safeDelete ? "Starting safe source deletion…" : verify ? "Starting verification…" : "Starting transfer…");
      ui["run-panel"].scrollIntoView({ block: "nearest" });
    } catch (error) {
      message("form-error", error.message);
    } finally {
      busy = false;
      if (attemptedStart) {
        statusEpoch++;
        statusKnown = false;
        await pollStatus();
      }
      updateControls();
    }
  }

  byId("transfer-form").addEventListener("submit", (event) => {
    event.preventDefault();
    startTransfer(false);
  });
  ui.verify.addEventListener("click", () => startTransfer(true));
  ui["safe-delete"].addEventListener("click", () => startTransfer(false, true));
  ui["quick-delete"].addEventListener("click", () => startTransfer(false, false, true));
  ui["refresh-devices"].addEventListener("click", refreshDevices);
  ui.device.addEventListener("change", selectDevice);
  ui["choose-source"].addEventListener("click", () => browse(ui["phone-path"].value || "/sdcard"));
  ui["phone-path"].addEventListener("input", () => {
    browseSequence++;
    browseController?.abort();
    browsing = false;
    browsedPath = "";
    folders = [];
    folderPage = 0;
    renderFolders();
    message("sources-error", "");
    message("browse-error", "");
    ui["phone-path"].removeAttribute("aria-invalid");
    setText(ui["browse-status"], "Path changed. Select Choose folder to browse this folder.");
    updateControls();
  });
  ui["phone-path"].addEventListener("keydown", (event) => {
    if (event.key === "Enter") {
      event.preventDefault();
      browse(ui["phone-path"].value);
    }
  });
  ui["up-folder"].addEventListener("click", () => {
    const path = browsedPath.replace(/\/+$/, "");
    browse(path.slice(0, path.lastIndexOf("/")) || "/");
  });
  ui["previous-folders"].addEventListener("click", () => {
    folderPage--;
    renderFolders();
    ui["folder-list"].scrollTop = 0;
  });
  ui["next-folders"].addEventListener("click", () => {
    folderPage++;
    renderFolders();
    ui["folder-list"].scrollTop = 0;
  });
  ui["choose-destination"].addEventListener("click", async () => {
    busy = true;
    message("destination-error", "");
    setText(ui["choose-destination"], "Choosing folder…");
    updateControls();
    try {
      const result = await request("/api/destination", undefined, { method: "POST", timeout: 0 });
      if (result.path) {
        ui.destination.value = result.path;
        ui.destination.removeAttribute("aria-invalid");
      }
    } catch (error) {
      message("destination-error", `${error.message} You can also enter the destination path above.`);
    } finally {
      busy = false;
      setText(ui["choose-destination"], "Choose folder…");
      updateControls();
      ui.destination.focus();
    }
  });
  ui["download-report"].addEventListener("click", async () => {
    setDisabled(ui["download-report"], true);
    message("report-error", "");
    try {
      const report = await request("/api/report", undefined, { download: true, timeout: 0 });
      const url = URL.createObjectURL(report.blob);
      const link = document.createElement("a");
      link.href = url;
      link.download = report.filename;
      document.body.append(link);
      link.click();
      link.remove();
      setTimeout(() => URL.revokeObjectURL(url), 1000);
    } catch (error) {
      message("report-error", error.message);
    } finally {
      setDisabled(ui["download-report"], false);
    }
  });
  ui.stop.addEventListener("click", async () => {
    if (stopping) return;
    stopping = true;
    statusEpoch++;
    message("stop-error", "");
    updateControls();
    try {
      await request("/api/stop", undefined, { method: "POST" });
      runState = "stopping";
      setText(ui["run-status"], "Stopping safely. Changes already completed will not be undone.");
    } catch (error) {
      message("stop-error", `${error.message} Stop could not be confirmed; check the transfer status before disconnecting the phone.`);
    } finally {
      statusEpoch++;
      stopping = false;
      await pollStatus();
      updateControls();
    }
  });
  activity.addEventListener("toggle", renderLog);
  document.addEventListener("visibilitychange", () => {
    clearTimeout(pollTimer);
    if (!document.hidden) pollStatus();
  });
  updateControls();
  pollStatus();
  refreshDevices();
})();
