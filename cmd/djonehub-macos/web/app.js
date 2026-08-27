const $ = (selector) => document.querySelector(selector);
let lastSMSCount = null;
let esimHealthPollTimer = null;
let esimHealthInFlight = false;
let networkTrafficTimer = null;
let networkTrafficPrevious = null;
let networkTrafficInFlight = false;
let callPollInFlight = false;
let voiceAgentCallsInFlight = false;
let voiceSetupInFlight = false;
let currentVoiceSetup = {};
let currentCallState = null;
let currentAudioHostState = {};
let currentVoiceAgentStatus = {};
let voiceAgentFormDirty = false;
let voiceAgentLoadedDevice = null;
let voiceAgentEventSource = null;
let liveVoiceAgentEvents = [];
let renderedVoiceAgentCallsSignature = "";
let currentVoiceAgentCalls = [];
let selectedVoiceAgentCallID = "";
let activeDeviceID = localStorage.getItem("djonehub-active-device") || "";
let knownDevices = [];

function routedAPIPath(path) {
  if (!activeDeviceID || path === "/api/devices" || path.startsWith("/api/devices/")) return path;
  if (!path.startsWith("/api/")) return path;
  return `/api/devices/${encodeURIComponent(activeDeviceID)}${path.slice(4)}`;
}

function setThemePreference(theme) {
  if (theme === "light" || theme === "dark") {
    document.documentElement.dataset.theme = theme;
    localStorage.setItem("djonehub-theme", theme);
    localStorage.removeItem("vohive-theme");
  } else {
    delete document.documentElement.dataset.theme;
    localStorage.removeItem("djonehub-theme");
    localStorage.removeItem("vohive-theme");
  }
  document.querySelectorAll("[data-theme-option]").forEach((button) => {
    button.setAttribute("aria-pressed", String(button.dataset.themeOption === theme));
  });
}

const savedTheme = localStorage.getItem("djonehub-theme") || localStorage.getItem("vohive-theme");
setThemePreference(savedTheme === "light" || savedTheme === "dark" ? savedTheme : "auto");
document.querySelectorAll("[data-theme-option]").forEach((button) => {
  button.addEventListener("click", () => setThemePreference(button.dataset.themeOption));
});

const operatorNames = new Map([
  ["CHN-UNICOM", "中国联通"],
  ["CHINA UNICOM", "中国联通"],
  ["UNICOM", "中国联通"],
  ["46001", "中国联通"],
  ["46006", "中国联通"],
  ["46009", "中国联通"],
  ["CHINA MOBILE", "中国移动"],
  ["CMCC", "中国移动"],
  ["CHN-CMCC", "中国移动"],
  ["46000", "中国移动"],
  ["46002", "中国移动"],
  ["46004", "中国移动"],
  ["46007", "中国移动"],
  ["46008", "中国移动"],
  ["CHINA TELECOM", "中国电信"],
  ["CHN-CT", "中国电信"],
  ["CTCC", "中国电信"],
  ["46003", "中国电信"],
  ["46005", "中国电信"],
  ["46011", "中国电信"],
  ["CBN", "中国广电"],
  ["CHN-CBN", "中国广电"],
  ["CHINA BROADNET", "中国广电"],
  ["46015", "中国广电"],
]);

async function api(path, options = {}) {
  const response = await fetch(routedAPIPath(path), {
    ...options,
    headers: { "Content-Type": "application/json", ...(options.headers || {}) },
  });
  const data = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(data.error || `HTTP ${response.status}`);
  return data;
}

function deviceOptionLabel(device) {
  const state = device.state === "ready"
    ? "在线"
    : (device.state === "reconnecting" ? "重新连接中" : "离线");
  const identity = device.imei_masked || device.physical_id || "未识别";
  return `${device.alias || "未命名模块"} · ${state} · ${identity}`;
}

async function loadDevices({ refreshCurrent = false, refreshOnChange = true } = {}) {
  const select = $("#device-select");
  try {
    const devices = await api("/api/devices");
    knownDevices = Array.isArray(devices) ? devices : [];
    const ready = knownDevices.filter((device) => device.state === "ready");
    const activeIsKnown = knownDevices.some((device) => device.id === activeDeviceID);
    // Preserve selection while a module briefly re-enumerates. Switching to a
    // different ready module here would make the UI appear to cross devices.
    if (!activeIsKnown) {
      activeDeviceID = ready[0]?.id || "";
      if (activeDeviceID) localStorage.setItem("djonehub-active-device", activeDeviceID);
      else localStorage.removeItem("djonehub-active-device");
      if (refreshOnChange) refreshCurrent = true;
    }
    select.replaceChildren();
    if (!knownDevices.length) {
      const option = document.createElement("option");
      option.value = "";
      option.textContent = "未检测到模块";
      select.append(option);
      select.disabled = true;
    } else {
      knownDevices.forEach((device) => {
        const option = document.createElement("option");
        option.value = device.id;
        option.textContent = deviceOptionLabel(device);
        option.disabled = device.state !== "ready";
        select.append(option);
      });
      select.disabled = ready.length === 0;
      select.value = activeDeviceID;
    }
    $("#rename-device").disabled = !activeDeviceID;
    if (refreshCurrent) await refreshActiveDevice();
  } catch (error) {
    // Older/single-serial/demo backends do not expose the multi-device list.
    // Clear a previously saved selection so their legacy /api/* routes keep
    // working without requiring a coordinated upgrade.
    activeDeviceID = "";
    localStorage.removeItem("djonehub-active-device");
    select.replaceChildren(new Option("当前单模块", ""));
    select.disabled = true;
    $("#rename-device").disabled = true;
  }
}

async function refreshActiveDevice() {
  lastSMSCount = null;
  networkTrafficPrevious = null;
  voiceAgentFormDirty = false;
  voiceAgentLoadedDevice = null;
  renderedVoiceAgentCallsSignature = "";
  currentVoiceAgentCalls = [];
  closeVoiceAgentCallDetail();
  closeVoiceAgentEvents();
  const tasks = [loadStatus(), loadSMS(), loadBarkSettings("sms")];
  if ($("#calls").classList.contains("active")) tasks.push(loadCalls(), loadVoiceAgentCalls());
  if ($("#esim").classList.contains("active")) tasks.push(loadESIM());
  if ($("#network").classList.contains("active")) tasks.push(loadNetwork());
  await Promise.allSettled(tasks);
  if ($("#calls").classList.contains("active")) connectVoiceAgentEvents();
}

function notice(message) {
  const el = $("#notice");
  el.textContent = message;
  el.classList.add("show");
  clearTimeout(notice.timer);
  notice.timer = setTimeout(() => el.classList.remove("show"), 2600);
}

let modalResolve = null;

function closeModal(result = null) {
  const modal = $("#app-modal");
  modal.hidden = true;
  document.body.classList.remove("modal-open");
  if (modalResolve) {
    const resolve = modalResolve;
    modalResolve = null;
    resolve(result);
  }
}

function showModal({ title, message = "", fields = [], confirmLabel = "确定", danger = false }) {
  if (modalResolve) closeModal(null);
  const modal = $("#app-modal");
  const messageElement = $("#modal-message");
  const fieldsElement = $("#modal-fields");
  const confirmButton = $("#modal-confirm");
  $("#modal-title").textContent = title;
  messageElement.textContent = message;
  messageElement.hidden = !message;
  fieldsElement.replaceChildren(...fields.map((field) => {
    const label = document.createElement("label");
    label.className = "modal-field";
    const caption = document.createElement("span");
    caption.textContent = field.label;
    const input = document.createElement("input");
    input.name = field.name;
    input.value = field.value || "";
    input.placeholder = field.placeholder || "";
    input.autocomplete = "off";
    if (field.required) input.required = true;
    if (field.maxLength) input.maxLength = field.maxLength;
    label.append(caption, input);
    return label;
  }));
  confirmButton.textContent = confirmLabel;
  confirmButton.className = danger ? "danger modal-danger" : "";
  modal.hidden = false;
  document.body.classList.add("modal-open");
  const firstInput = fieldsElement.querySelector("input");
  setTimeout(() => (firstInput || confirmButton).focus(), 0);
  return new Promise((resolve) => { modalResolve = resolve; });
}

$("#modal-form").addEventListener("submit", (event) => {
  event.preventDefault();
  const values = {};
  event.currentTarget.querySelectorAll(".modal-fields input").forEach((input) => {
    values[input.name] = input.value.trim();
  });
  closeModal(values);
});
$("#modal-cancel").addEventListener("click", () => closeModal(null));
$("#modal-close").addEventListener("click", () => closeModal(null));
$("#app-modal").addEventListener("click", (event) => {
  if (event.target === event.currentTarget) closeModal(null);
});
document.addEventListener("keydown", (event) => {
  if (event.key === "Escape" && !$("#app-modal").hidden) closeModal(null);
});

async function copySMSCode(code) {
  try {
    await navigator.clipboard.writeText(code);
    notice(`验证码 ${code} 已复制`);
  } catch (error) {
    notice("复制失败，请手动复制验证码");
  }
}

function renderHardwareDetails(status) {
  const panel = $("#hardware-details");
  const device = status.usb_device;
  if (!device && !status.discovery_error) {
    panel.hidden = true;
    panel.replaceChildren();
    return;
  }

  const title = document.createElement("strong");
  title.textContent = device ? "已检测到大疆 USB 设备" : "未检测到可用硬件";

  const detail = document.createElement("p");
  if (device) {
    const interfaceText = Array.isArray(device.interfaces)
      ? `${device.interfaces.length} 个 USB interface`
      : "interface 未知";
    detail.textContent = [
      `${device.vendor || "DJI"} ${device.product || ""}`.trim(),
      `${device.vendor_id}:${device.product_id}`,
      device.mode,
      interfaceText,
    ].filter(Boolean).join(" · ");
  } else {
    detail.textContent = status.discovery_error || "设备未枚举";
  }

  const hint = document.createElement("small");
  hint.textContent = status.discovery_error
    ? `当前限制：${status.discovery_error}`
    : "AT 串口可用后，短信、来电和 eSIM/卡片操作会自动启用。";

  panel.hidden = false;
  panel.replaceChildren(title, detail, hint);
}

function setValue(id, text, tone = "") {
  const el = $(id);
  el.textContent = text || "--";
  el.className = tone;
}

function displayOperatorName(value) {
  const raw = String(value || "").trim();
  if (!raw) return "--";
  return operatorNames.get(raw.toUpperCase()) || raw;
}

function displayWorkMode(value) {
	if (value === null || value === undefined || value === "") {
	  return { label: "待读取", tone: "muted" };
	}
  switch (Number(value)) {
    case 0: return { label: "短信模式", tone: "info" };
    case 1: return { label: "上网模式", tone: "info" };
    case 2: return { label: "实验模式 2", tone: "warn" };
    case 3: return { label: "实验模式 3", tone: "warn" };
    default: return { label: "待读取", tone: "muted" };
  }
}

function signalTone(dbm) {
  const value = Number(dbm);
  if (!Number.isFinite(value) || value === 0) return "muted";
  if (value >= -65) return "good";
  if (value >= -75) return "signal-fair";
  if (value >= -85) return "warn";
  if (value >= -95) return "orange";
  return "bad";
}

async function loadStatus() {
  try {
    const status = await api("/api/status");
    setValue("#operator", displayOperatorName(status.operator), status.operator ? "info" : "muted");
    setValue("#signal", status.signal_dbm ? `${status.signal_dbm} dBm` : "--", signalTone(status.signal_dbm));
    setValue("#network-mode", status.network_mode || status.reg_status_text || "--", status.network_mode ? "info" : "muted");
    setValue(
      "#sim",
      status.sim_inserted ? "已插入" : (status.usb_device ? "待读取" : "未检测到"),
      status.sim_inserted ? "good" : (status.usb_device ? "warn" : "bad"),
    );
    const workMode = Object.prototype.hasOwnProperty.call(status, "usbnet_mode")
      ? displayWorkMode(status.usbnet_mode)
      : displayWorkMode(null);
    setValue("#work-mode", workMode.label, workMode.tone);
    $("#device-summary").textContent =
      status.hardware_status || [status.imei, status.firmware].filter(Boolean).join(" · ") || "模块初始化中";
    renderHardwareDetails(status);
  } catch (error) {
    $("#device-summary").textContent = error.message;
  }
}

async function loadSMS() {
  const list = $("#sms-list");
  try {
    const [messages, status] = await Promise.all([
      api("/api/sms"),
      api("/api/sms/status"),
    ]);
    const pollText = status.polling
      ? `自动轮询 ${status.poll_interval_s || 8}s`
      : "自动轮询未启用";
    const cleanupText = status.auto_cleanup_me ? "自动清理 ME 已开启" : "自动清理 ME 未开启";
    const errorText = status.last_poll_error ? ` · 最近错误：${status.last_poll_error}` : "";
    $("#sms-status").textContent = `当前缓存 ${messages.length} 条短信 · ${pollText} · ${cleanupText}${errorText}`;
    if (lastSMSCount !== null && messages.length > lastSMSCount) {
      notice(`收到 ${messages.length - lastSMSCount} 条新短信`);
    }
    lastSMSCount = messages.length;
    if (!messages.length) {
      list.className = "list empty";
      list.textContent = "暂无短信";
      return;
    }
    list.className = "list";
    list.replaceChildren(...messages.map((message) => {
      const row = document.createElement("article");
      row.className = "item";
      const sender = document.createElement("strong");
      sender.textContent = message.sender || "未知号码";
      const content = document.createElement("p");
      content.textContent = message.content;
      const time = document.createElement("time");
      time.textContent = new Date(message.timestamp).toLocaleString();
      if (message.code) {
        const actions = document.createElement("div");
        actions.className = "sms-actions";
        const badge = document.createElement("span");
        badge.className = "code-badge";
        badge.textContent = `验证码 ${message.code}`;
        const copy = document.createElement("button");
        copy.className = "secondary compact";
        copy.type = "button";
        copy.textContent = "复制";
        copy.addEventListener("click", () => copySMSCode(message.code));
        actions.append(badge, copy, time);
        row.append(sender, content, actions);
      } else {
        row.append(sender, content, time);
      }
      return row;
    }));
  } catch (error) {
    $("#sms-status").textContent = `读取列表失败：${error.message}`;
    notice(error.message);
  }
}

function barkEndpoint(kind) {
  return kind === "call"
    ? "/api/settings/bark/missed-call"
    : "/api/settings/bark/sms";
}

function renderBarkSettings(kind, settings) {
  const prefix = kind === "call" ? "call" : "sms";
  const label = kind === "call" ? "来电" : "短信";
  const enabled = Boolean(settings?.enabled);
  $(`#${prefix}-bark-enabled`).checked = enabled;
  $(`#${prefix}-bark-api-url`).value = settings?.api_url || "";
  $(`#${prefix}-bark-alias`).value = settings?.alias || "";
  $(`#${prefix}-bark-message-template`).value = settings?.message_template || "";
  $(`#${prefix}-bark-summary`).textContent = enabled ? "已启用" : (settings?.api_url ? "已停用" : "未配置");
  $(`#${prefix}-bark-status`).textContent = enabled
    ? `${label} Bark 转发已启用。`
    : `${label} Bark 转发当前未启用。`;
}

async function loadBarkSettings(kind) {
  const prefix = kind === "call" ? "call" : "sms";
  try {
    renderBarkSettings(kind, await api(barkEndpoint(kind)));
  } catch (error) {
    $(`#${prefix}-bark-status`).textContent = `读取 Bark 配置失败：${error.message}`;
  }
}

async function saveBarkSettings(event, kind) {
  event.preventDefault();
  const prefix = kind === "call" ? "call" : "sms";
  const button = event.submitter || event.currentTarget.querySelector("button[type=submit]");
  const settings = {
    enabled: $(`#${prefix}-bark-enabled`).checked,
    api_url: $(`#${prefix}-bark-api-url`).value.trim(),
    alias: $(`#${prefix}-bark-alias`).value.trim(),
    message_template: $(`#${prefix}-bark-message-template`).value.trim(),
  };
  button.disabled = true;
  $(`#${prefix}-bark-status`).textContent = "正在保存 Bark 配置...";
  try {
    const result = await api(barkEndpoint(kind), {
      method: "PUT",
      body: JSON.stringify(settings),
    });
    renderBarkSettings(kind, result.settings || settings);
    notice("Bark 通知配置已保存");
  } catch (error) {
    $(`#${prefix}-bark-status`).textContent = `保存失败：${error.message}`;
    notice(error.message);
  } finally {
    button.disabled = false;
  }
}

async function testBarkSettings(kind) {
  const prefix = kind === "call" ? "call" : "sms";
  const button = kind === "call" ? $("#test-call-bark") : $("#test-sms-bark");
  button.disabled = true;
  $(`#${prefix}-bark-status`).textContent = "正在发送 Bark 测试通知...";
  try {
    const result = await api(`${barkEndpoint(kind)}/test`, { method: "POST" });
    $(`#${prefix}-bark-status`).textContent = result.message || "Bark 测试通知已发送。";
    notice(result.message || "Bark 测试通知已发送");
  } catch (error) {
    $(`#${prefix}-bark-status`).textContent = `测试失败：${error.message}`;
    notice(error.message);
  } finally {
    button.disabled = false;
  }
}

function callStateLabel(call) {
  switch (call?.state) {
    case "incoming": return "正在来电";
    case "waiting": return "来电等待";
    case "active": return "通话已接通";
    case "dialing": return "正在拨号";
    case "alerting": return "等待接听";
    case "held": return "通话保持";
    default: return "通话状态";
  }
}

function callDuration(call) {
  if (!call?.started_at || !call?.ended_at) return "";
  const seconds = Math.max(0, Math.round((new Date(call.ended_at) - new Date(call.started_at)) / 1000));
  const hours = Math.floor(seconds / 3600);
  const minutes = Math.floor((seconds % 3600) / 60);
  const rest = seconds % 60;
  if (hours) return `${hours} 小时 ${minutes} 分 ${rest} 秒`;
  if (minutes) return `${minutes} 分 ${rest} 秒`;
  return `${rest} 秒`;
}

function renderCallHistory(history) {
  const list = $("#call-history");
  const rows = Array.isArray(history) ? history : [];
  $("#call-history-count").textContent = `${rows.length} 条`;
  if (!rows.length) {
    list.className = "list empty";
    list.textContent = "暂无记录";
    return;
  }
  list.className = "list";
  list.replaceChildren(...rows.map((call) => {
    const row = document.createElement("article");
    row.className = `item call-history-item${call.missed ? " missed" : ""}`;
    const number = document.createElement("strong");
    number.textContent = call.number || "未知号码";
    const state = document.createElement("p");
    const result = call.missed
      ? "未接来电"
      : (call.ai_handled ? "AI 已接听" : (call.direction === "incoming" ? "已接来电" : "外呼"));
    state.textContent = [result, callDuration(call)].filter(Boolean).join(" · ");
    const time = document.createElement("time");
    time.textContent = new Date(call.started_at).toLocaleString();
    row.append(number, state, time);
    return row;
  }));
}

function voiceAgentEventLabel(type) {
  if (type.startsWith("transcript.input")) return "来电方";
  if (type.startsWith("transcript.output")) return "AI";
  if (type === "recording.started") return "录音开始";
  if (type === "recording.stopped") return "录音完成";
  if (type.startsWith("tool.")) return "工具";
  if (type === "error") return "错误";
  if (type === "speech.started") return "检测说话";
  if (type === "speech.stopped") return "停止说话";
  if (type === "session.ready") return "会话就绪";
  if (type === "session.closed") return "会话结束";
  if (type === "audio.done") return "播放就绪";
  if (type === "usage") return "用量";
  return type || "事件";
}

function voiceAgentToolArguments(argumentsValue) {
  if (argumentsValue == null || argumentsValue === "") return "";
  let parsed = argumentsValue;
  if (typeof parsed === "string") {
    try { parsed = JSON.parse(parsed); } catch (_) { return parsed; }
  }
  if (typeof parsed === "object" && !Array.isArray(parsed) && Object.keys(parsed).length === 0) return "";
  try { return JSON.stringify(parsed); } catch (_) { return String(parsed); }
}

function voiceAgentUsageText(usage) {
  if (!usage || typeof usage !== "object") return "用量统计未提供明细";
  const fields = [
    ["total_tokens", "总计"],
    ["input_tokens", "输入"],
    ["output_tokens", "输出"],
  ];
  const parts = fields
    .filter(([key]) => usage[key] != null && Number.isFinite(Number(usage[key])))
    .map(([key, label]) => `${label} ${usage[key]}`);
  if (parts.length) return `${parts.join(" · ")} tokens`;
  try { return JSON.stringify(usage); } catch (_) { return "用量统计格式无法解析"; }
}

function voiceAgentEventText(event) {
  if (event.error) return event.error;
  if (String(event.type || "").startsWith("recording.")) {
    const normalizedPath = String(event.text || "").replaceAll("\\", "/");
    return normalizedPath.split("/").filter(Boolean).at(-1) || "通话录音.wav";
  }
  if (String(event.type || "").startsWith("transcript.")) {
    return event.text || (String(event.type).endsWith(".final") ? "未识别到有效语音" : "正在识别…");
  }
  if (event.type === "usage") return voiceAgentUsageText(event.usage);
  if (event.text) return event.text;
  if (event.tool_call) {
    return `${event.tool_call.name || "未知工具"} ${voiceAgentToolArguments(event.tool_call.arguments)}`.trim();
  }
  if (event.type === "session.ready") return `已连接 ${event.provider || "Provider"}`;
  if (event.type === "session.closed") return "语音会话已关闭";
  if (event.type === "speech.started") return "检测到来电方开始说话，已打断待播放语音";
  if (event.type === "speech.stopped") return "来电方本轮说话结束";
  if (event.type === "audio.done") return "本轮 AI 语音已全部进入播放队列";
  return event.provider || "";
}

function renderVoiceAgentEvents(target, events, emptyText) {
  if (!events.length) {
    target.className = "agent-transcript empty";
    target.textContent = emptyText;
    return;
  }
  target.className = "agent-transcript";
  target.replaceChildren(...events.map((event) => {
    const row = document.createElement("article");
    const type = String(event.type || "");
    row.className = `agent-event${type === "error" ? " error" : ""}${type.startsWith("tool.") ? " tool" : ""}`;
    const role = document.createElement("span");
    role.className = "agent-event-role";
    role.textContent = voiceAgentEventLabel(type);
    const text = document.createElement("div");
    text.className = "agent-event-text";
    text.textContent = voiceAgentEventText(event) || "—";
    if (type.startsWith("recording.") && event.text) {
      const actions = document.createElement("div");
      actions.className = "agent-recording-actions";
      if (type === "recording.stopped" && event.sequence) {
        const player = document.createElement("audio");
        player.className = "agent-recording-player";
        player.controls = true;
        player.preload = "none";
        player.src = routedAPIPath(`/api/voice-agent/audit/${encodeURIComponent(event.sequence)}/recording`);
        player.setAttribute("aria-label", `播放${voiceAgentEventText(event)}`);
        actions.append(player);
      }
      const copyPath = document.createElement("button");
      copyPath.className = "secondary compact";
      copyPath.type = "button";
      copyPath.textContent = "复制路径";
      copyPath.addEventListener("click", () => copyIdentifier(event.text, "录音路径"));
      actions.append(copyPath);
      text.append(actions);
    }
    const at = document.createElement("time");
    at.textContent = event.at ? new Date(event.at).toLocaleTimeString() : "";
    row.append(role, text, at);
    return row;
  }));
  target.scrollTop = target.scrollHeight;
}

function appendLiveVoiceAgentEvent(event) {
  const type = String(event.type || "");
  const streamKind = type.startsWith("transcript.input") ? "input" : (type.startsWith("transcript.output") ? "output" : "");
  const final = type.endsWith(".final");
  const delta = type.endsWith(".delta");
  const last = liveVoiceAgentEvents.at(-1);
  if (delta && last?.streamKind === streamKind && !last.final) {
    last.text = `${last.text || ""}${event.text || ""}`;
    last.at = event.at || last.at;
  } else if (final && last?.streamKind === streamKind && !last.final) {
    last.text = event.text || last.text;
    last.type = type;
    last.final = true;
    last.at = event.at || last.at;
  } else {
    liveVoiceAgentEvents.push({ ...event, streamKind, final });
  }
  if (liveVoiceAgentEvents.length > 120) liveVoiceAgentEvents = liveVoiceAgentEvents.slice(-120);
  renderVoiceAgentEvents($("#voice-agent-transcript"), liveVoiceAgentEvents, "等待 Voice Agent 事件...");
  if (type.startsWith("tool.")) void loadVoiceAgentPendingTools();
}

function closeVoiceAgentEvents() {
  if (voiceAgentEventSource) voiceAgentEventSource.close();
  voiceAgentEventSource = null;
  $("#voice-agent-stream-status").textContent = "事件流未连接";
}

function connectVoiceAgentEvents() {
  closeVoiceAgentEvents();
  if (!$("#calls").classList.contains("active")) return;
  const source = new EventSource(routedAPIPath("/api/voice-agent/events"));
  voiceAgentEventSource = source;
  source.addEventListener("ready", () => {
    $("#voice-agent-stream-status").textContent = "实时事件流已连接";
  });
  source.addEventListener("voice-agent", (message) => {
    try {
      const event = JSON.parse(message.data);
      appendLiveVoiceAgentEvent(event);
      if (event.type === "recording.stopped") void loadVoiceAgentCalls();
    } catch (_) {}
  });
  source.onerror = () => {
    $("#voice-agent-stream-status").textContent = "事件流中断，正在自动重连";
  };
}

function updateVoiceAgentFieldVisibility() {
  const provider = $("#voice-agent-provider").value;
  const isMiniMax = provider === "minimax"
    || $("#voice-agent-fallback-provider").value === "minimax";
  document.querySelectorAll(".minimax-setting").forEach((field) => { field.hidden = !isMiniMax; });
  const choices = provider === "openai"
    ? ["marin", "cedar", "alloy", "ash", "ballad", "coral", "echo", "sage", "shimmer", "verse"]
    : (provider === "qwen" ? ["Cherry"] : []);
  $("#voice-agent-voice-options").replaceChildren(...choices.map((value) => {
    const option = document.createElement("option");
    option.value = value;
    return option;
  }));
  $("#voice-agent-voice").placeholder = provider === "openai"
    ? "marin（推荐）或 cedar"
    : (provider === "qwen" ? "Cherry" : "例如 male-qn-qingse");
}

function normalizeVoiceAgentVoiceForProvider() {
  const provider = $("#voice-agent-provider").value;
  const field = $("#voice-agent-voice");
  const openAIVoices = new Set(["alloy", "ash", "ballad", "coral", "echo", "sage", "shimmer", "verse", "marin", "cedar"]);
  if (provider === "openai" && !openAIVoices.has(field.value.trim().toLowerCase())) field.value = "marin";
  if (provider === "qwen" && (!field.value.trim() || openAIVoices.has(field.value.trim().toLowerCase()))) field.value = "Cherry";
}

function populateVoiceAgentForm(state) {
  $("#voice-agent-enabled").checked = Boolean(state.enabled);
  $("#voice-agent-provider").value = state.provider || "qwen";
  $("#voice-agent-fallback-provider").value = state.fallback_provider || "";
  $("#voice-agent-model").value = state.model || "";
  $("#voice-agent-voice").value = state.voice || "";
  $("#voice-agent-stt-provider").value = state.stt_provider || "qwen";
  $("#voice-agent-fallback-stt-provider").value = state.fallback_stt_provider || "";
  $("#voice-agent-stt-model").value = state.stt_model || "";
  $("#voice-agent-instructions").value = state.instructions || "";
  $("#voice-agent-tools-enabled").checked = Boolean(state.tools_enabled);
  $("#voice-agent-audit-enabled").checked = Boolean(state.audit_enabled);
  $("#voice-agent-redact-pii").checked = state.redact_pii !== false;
  $("#voice-agent-auto-answer").checked = Boolean(state.auto_answer);
  $("#voice-agent-auto-answer-delay").value = state.auto_answer_delay_ms || 1200;
  updateVoiceAgentFieldVisibility();
}

function renderVoiceAgentProviders(status) {
  const displayName = (name) => name === "qwen" ? "Qwen" : (name === "openai" ? "OpenAI" : (name === "minimax" ? "MiniMax" : name));
  const readiness = [
    ...(status.providers || []).map((item) => ({ ...item, label: item.name === "minimax" ? "MiniMax LLM/TTS" : `${displayName(item.name)} Realtime` })),
    ...(status.stt_providers || []).map((item) => ({ ...item, label: `${displayName(item.name)} STT`, reason: item.configured ? "流式转写密钥已配置" : "缺少对应 API Key" })),
  ];
  $("#voice-agent-providers").replaceChildren(...readiness.map((item) => {
    const card = document.createElement("div");
    card.className = `agent-provider-card${item.configured ? " ready" : ""}`;
    const name = document.createElement("strong");
    name.textContent = item.label;
    const detail = document.createElement("small");
    detail.textContent = item.configured ? (item.mode ? `${item.mode} · 已就绪` : item.reason || "已就绪") : (item.reason || "尚未配置");
    card.append(name, detail);
    return card;
  }));
}

async function loadVoiceAgentStatus({ forcePopulate = false } = {}) {
  try {
    const status = await api("/api/voice-agent/status");
    currentVoiceAgentStatus = status;
    const state = status.state || {};
    const deviceContext = activeDeviceID || "single";
    if (forcePopulate || !voiceAgentFormDirty || voiceAgentLoadedDevice !== deviceContext) {
      populateVoiceAgentForm(state);
      voiceAgentLoadedDevice = deviceContext;
      voiceAgentFormDirty = false;
    }
    const badge = $("#voice-agent-connection");
    badge.className = `agent-status-badge${state.connected ? " connected" : (state.last_error ? " error" : "")}`;
    badge.textContent = state.connected
      ? `已连接 · ${state.active_provider || state.provider}`
      : (state.enabled ? "Agent 待机" : "人工模式");
    $("#voice-agent-status").textContent = state.last_error
      ? `最近错误：${state.last_error}`
      : (state.enabled
        ? `${state.provider || "未选择"} 已启用${state.reconnect_count ? ` · 本次通话重连 ${state.reconnect_count} 次` : ""}`
        : "当前使用本地麦克风；保存启用后，活动通话会自动切换媒体。");
    renderVoiceAgentProviders(status);
  } catch (error) {
    $("#voice-agent-status").textContent = `Voice Agent 状态读取失败：${error.message}`;
  }
}

async function saveVoiceAgentProfile({ forceDisabled = false } = {}) {
  const form = $("#voice-agent-form");
  const submit = form.querySelector("button[type=submit]");
  submit.disabled = true;
  try {
    const result = await api("/api/voice-agent/config", { method: "PUT", body: JSON.stringify({
      enabled: forceDisabled ? false : $("#voice-agent-enabled").checked,
      provider: $("#voice-agent-provider").value,
      fallback_provider: $("#voice-agent-fallback-provider").value,
      model: $("#voice-agent-model").value.trim(),
      voice: $("#voice-agent-voice").value.trim(),
      stt_provider: $("#voice-agent-stt-provider").value,
      fallback_stt_provider: $("#voice-agent-fallback-stt-provider").value,
      stt_model: $("#voice-agent-stt-model").value.trim(),
      instructions: $("#voice-agent-instructions").value.trim(),
      tools_enabled: $("#voice-agent-tools-enabled").checked,
      audit_enabled: $("#voice-agent-audit-enabled").checked,
      redact_pii: $("#voice-agent-redact-pii").checked,
      auto_answer: $("#voice-agent-auto-answer").checked,
      auto_answer_delay_ms: Number($("#voice-agent-auto-answer-delay").value || 1200),
    }) });
    voiceAgentFormDirty = false;
    populateVoiceAgentForm(result);
    notice(forceDisabled ? "已切回人工麦克风模式" : "Voice Agent Profile 已保存");
    await loadVoiceAgentStatus({ forcePopulate: true });
  } catch (error) {
    notice(error.message);
  } finally {
    submit.disabled = false;
  }
}

async function loadVoiceAgentPendingTools() {
  try {
    const result = await api("/api/voice-agent/tools/pending");
    const tools = result.tools || [];
    const panel = $("#voice-agent-pending");
    panel.hidden = tools.length === 0;
    $("#voice-agent-pending-list").replaceChildren(...tools.map((tool) => {
      const row = document.createElement("div");
      row.className = "agent-pending-item";
      const description = document.createElement("div");
      const title = document.createElement("strong");
      title.textContent = tool.summary || tool.name;
      const expiry = document.createElement("small");
      expiry.textContent = `到期：${new Date(tool.expires_at).toLocaleTimeString()}`;
      description.append(title, expiry);
      const actions = document.createElement("div");
      actions.className = "agent-pending-actions";
      const reject = document.createElement("button");
      reject.className = "secondary danger compact";
      reject.type = "button";
      reject.textContent = "拒绝";
      const approve = document.createElement("button");
      approve.className = "compact";
      approve.type = "button";
      approve.textContent = "确认执行";
      const decide = async (approved) => {
        reject.disabled = approve.disabled = true;
        try {
          await api(`/api/voice-agent/tools/${encodeURIComponent(tool.id)}`, { method: "POST", body: JSON.stringify({ approve: approved }) });
          notice(approved ? "工具操作已确认" : "工具操作已拒绝");
        } catch (error) { notice(error.message); }
        await loadVoiceAgentPendingTools();
      };
      reject.addEventListener("click", () => decide(false));
      approve.addEventListener("click", () => decide(true));
      actions.append(reject, approve);
      row.append(description, actions);
      return row;
    }));
  } catch (_) {}
}

function renderVoiceAgentCallMessages(target, call) {
  const messages = Array.isArray(call.messages) ? call.messages : [];
  if (!messages.length) {
    const empty = document.createElement("div");
    empty.className = "agent-call-empty-transcript";
    empty.textContent = call.ended_at ? "本通电话没有识别到有效通话内容" : "正在等待双方通话内容…";
    target.replaceChildren(empty);
    return;
  }
  target.replaceChildren(...messages.map((message) => {
    const row = document.createElement("div");
    row.className = "agent-call-message";
    const role = document.createElement("span");
    role.className = "agent-call-message-role";
    role.textContent = message.role === "ai" ? "AI" : "来电方";
    const text = document.createElement("p");
    text.textContent = message.text || "—";
    const at = document.createElement("time");
    at.textContent = message.at ? new Date(message.at).toLocaleTimeString() : "";
    row.append(role, text, at);
    return row;
  }));
}

function renderVoiceAgentCallRecording(target, call) {
  if (call.recording_available) {
    const endpoint = `/api/voice-agent/calls/${encodeURIComponent(call.call_id)}/recording`;
    const player = document.createElement("audio");
    player.controls = true;
    player.preload = "none";
    player.src = routedAPIPath(endpoint);
    player.setAttribute("aria-label", `播放 ${call.number || "本通电话"} 的通话录音`);
    const download = document.createElement("a");
    download.href = `${routedAPIPath(endpoint)}?download=1`;
    download.textContent = "下载录音";
    download.setAttribute("download", call.recording_filename || "AI通话录音.wav");
    target.replaceChildren(player, download);
    return;
  }
  const pending = document.createElement("small");
  pending.textContent = call.ended_at ? "录音文件尚未生成或已被移除" : "通话结束后可播放和下载完整录音";
  target.replaceChildren(pending);
}

function voiceAgentCallMetaItem(label, value) {
  const item = document.createElement("div");
  const name = document.createElement("span");
  name.textContent = label;
  const content = document.createElement("strong");
  content.textContent = value || "—";
  item.append(name, content);
  return item;
}

function renderVoiceAgentCallDetail(call) {
  const messages = Array.isArray(call.messages) ? call.messages : [];
  $("#voice-agent-call-detail-title").textContent = call.number || "未知号码";
  $("#voice-agent-call-detail-status").textContent = call.ended_at ? "已结束" : "通话中";
  $("#voice-agent-call-detail-status").className = `agent-status-badge${call.ended_at ? "" : " connected"}`;
  $("#voice-agent-call-detail-message-count").textContent = `${messages.length} 条`;
  $("#voice-agent-call-detail-meta").replaceChildren(
    voiceAgentCallMetaItem("开始时间", call.started_at ? new Date(call.started_at).toLocaleString() : "—"),
    voiceAgentCallMetaItem("通话时长", call.ended_at ? callDuration(call) : "通话中"),
    voiceAgentCallMetaItem("AI Provider", call.provider || "默认 Provider"),
    voiceAgentCallMetaItem("通话方向", call.direction === "outgoing" ? "AI 外呼" : "AI 接听"),
  );
  renderVoiceAgentCallMessages($("#voice-agent-call-detail-messages"), call);
  renderVoiceAgentCallRecording($("#voice-agent-call-detail-recording"), call);
}

function openVoiceAgentCallDetail(callID) {
  const call = currentVoiceAgentCalls.find((item) => item.call_id === callID);
  if (!call) return;
  selectedVoiceAgentCallID = callID;
  renderVoiceAgentCallDetail(call);
  $("#calls-overview-intro").hidden = true;
  $("#call-module-list").hidden = true;
  $("#voice-agent-call-detail").hidden = false;
  $("#back-to-voice-agent-calls").focus({ preventScroll: true });
  $("#calls").scrollIntoView({ behavior: "smooth", block: "start" });
}

function closeVoiceAgentCallDetail() {
  selectedVoiceAgentCallID = "";
  const intro = $("#calls-overview-intro");
  const modules = $("#call-module-list");
  const detail = $("#voice-agent-call-detail");
  if (!intro || !modules || !detail) return;
  intro.hidden = false;
  modules.hidden = false;
  detail.hidden = true;
}

function renderVoiceAgentCalls(calls) {
  const list = $("#voice-agent-call-list");
  const rows = Array.isArray(calls) ? calls : [];
  currentVoiceAgentCalls = rows;
  const signature = JSON.stringify(rows);
  if (signature === renderedVoiceAgentCallsSignature) return;
  const detailPlayer = $("#voice-agent-call-detail-recording audio");
  if (detailPlayer && !detailPlayer.paused && !detailPlayer.ended) return;
  renderedVoiceAgentCallsSignature = signature;
  $("#voice-agent-call-count").textContent = `${rows.length} 通`;
  if (!rows.length) {
    list.className = "agent-call-list empty";
    list.textContent = "暂无 AI 通话记录";
    if (selectedVoiceAgentCallID) closeVoiceAgentCallDetail();
    return;
  }
  list.className = "agent-call-list";
  list.replaceChildren(...rows.map((call) => {
    const button = document.createElement("button");
    button.type = "button";
    button.className = "agent-call-list-item";
    button.setAttribute("aria-label", `查看 ${call.number || "未知号码"} 的 AI 通话详情`);
    const identity = document.createElement("span");
    identity.className = "agent-call-list-identity";
    const number = document.createElement("strong");
    number.textContent = call.number || "未知号码";
    const messages = Array.isArray(call.messages) ? call.messages : [];
    const meta = document.createElement("small");
    const status = call.ended_at ? callDuration(call) : "通话中";
    meta.textContent = [call.provider ? `AI · ${call.provider}` : "AI 接听", status, `${messages.length} 条转写`].filter(Boolean).join(" · ");
    const preview = document.createElement("span");
    preview.className = "agent-call-list-preview";
    preview.textContent = messages.length ? messages[messages.length - 1].text : (call.ended_at ? "没有识别到有效通话内容" : "正在等待通话内容…");
    identity.append(number, meta, preview);
    const started = document.createElement("time");
    started.className = "agent-call-list-time";
    started.textContent = call.started_at ? new Date(call.started_at).toLocaleString() : "";
    button.append(identity, started);
    button.addEventListener("click", () => openVoiceAgentCallDetail(call.call_id));
    return button;
  }));
  if (selectedVoiceAgentCallID) {
    const selected = rows.find((call) => call.call_id === selectedVoiceAgentCallID);
    if (selected) renderVoiceAgentCallDetail(selected);
    else closeVoiceAgentCallDetail();
  }
}

async function loadVoiceAgentCalls() {
  if (voiceAgentCallsInFlight) return;
  voiceAgentCallsInFlight = true;
  try {
    const result = await api("/api/voice-agent/calls?limit=50");
    renderVoiceAgentCalls(result.calls || []);
  } catch (error) {
    const list = $("#voice-agent-call-list");
    renderedVoiceAgentCallsSignature = "";
    list.className = "agent-call-list empty";
    list.textContent = error.message;
  } finally {
    voiceAgentCallsInFlight = false;
  }
}

async function loadCalls() {
  if (callPollInFlight) return;
  callPollInFlight = true;
  try {
    const status = await api("/api/calls/status");
    const active = status.active;
    const callJustEnded = currentCallState !== null && !active;
    const audioHost = status.audio_host || {};
    currentAudioHostState = audioHost;
    const panel = $("#active-call");
    const pollText = status.polling
      ? `每 ${status.poll_interval_s || 3} 秒检查`
      : "演示模式";
    const monitorStatus = $("#call-monitor-status");
    monitorStatus.classList.toggle("attention", Boolean(active || status.last_poll_error));
    monitorStatus.textContent = status.last_poll_error
      ? `监听异常 · ${status.last_poll_error}`
      : (active
        ? `${callStateLabel(active)} · ${active.number || "未知号码"}`
        : `${pollText} · 监听正常`);
    if (active) {
      currentCallState = active.state || null;
      panel.hidden = false;
      $("#active-call-label").textContent = callStateLabel(active);
      $("#active-call-number").textContent = active.number || "未知号码";
      $("#active-call-time").textContent = new Date(active.started_at).toLocaleString();
      const ringing = active.state === "incoming" || active.state === "waiting";
      $("#answer-call").hidden = !ringing;
      $("#reject-call").hidden = !ringing;
      $("#hangup-call").hidden = false;
      $("#dtmf-panel").hidden = active.state !== "active";
      const mediaActive = active.state === "active" && audioHost.registered;
      $("#mute-call").hidden = !mediaActive;
      $("#record-call").hidden = !mediaActive;
      $("#mute-call").textContent = audioHost.want_muted ? "取消静音" : "静音";
      $("#record-call").disabled = Boolean(active.ai_handled);
      $("#record-call").textContent = active.ai_handled ? "AI 自动录音中" : (audioHost.want_recording ? "停止录音" : "录音");
      $("#dial-call").disabled = true;
    } else {
      currentCallState = null;
      panel.hidden = true;
      $("#answer-call").hidden = true;
      $("#reject-call").hidden = true;
      $("#hangup-call").hidden = true;
      $("#dtmf-panel").hidden = true;
      $("#mute-call").hidden = true;
      $("#record-call").hidden = true;
      $("#record-call").disabled = false;
      $("#dial-call").disabled = false;
    }
    $("#audio-host-status").textContent = audioHost.registered
      ? (audioHost.running
        ? `Mac 双向音频已连接${audioHost.recording_path ? ` · 录音：${audioHost.recording_path}` : ""}`
        : `Mac 音频宿主在线${audioHost.error ? ` · ${audioHost.error}` : "，等待通话媒体"}`)
      : "Mac 音频宿主尚未连接；发行包请用 djonehub start，源码运行需同时启动 DJOneHubAudioHost";
    renderCallHistory(status.history);
    if (callJustEnded) void loadVoiceAgentCalls();
  } catch (error) {
    $("#call-monitor-status").textContent = `监听异常：${error.message}`;
    $("#call-monitor-status").classList.add("attention");
  } finally {
    callPollInFlight = false;
  }
  void loadVoiceSetup();
  void loadVoiceAgentStatus();
  void loadVoiceAgentPendingTools();
}

async function loadVoiceSetup() {
  if (voiceSetupInFlight) return;
  voiceSetupInFlight = true;
  const summary = $("#voice-setup-summary");
  const detail = $("#voice-setup-detail");
  const initializeButton = $("#initialize-voice-module");
  const installButton = $("#install-voice-runtime");
  try {
    const [setup, voice] = await Promise.all([
      api("/api/module/setup"),
      api("/api/voice/status"),
    ]);
    const setupText = setup.summary || setup.state || "模块配置状态未知";
    currentVoiceSetup = setup;
    const runtimeText = voice.runtime_installed
      ? (voice.ready
        ? "模块语音路由运行中"
        : (voice.preparing
          ? "正在空闲预热 ADB 与 ACDB"
          : (voice.prepared ? "ADB 与 ACDB 已准备" : "语音运行时已校验")))
      : "语音运行时尚未安装";
    summary.textContent = `${setupText} · ${runtimeText}`;
    detail.textContent = [setup.detail, voice.last_error || voice.detail || voice.runtime_detail].filter(Boolean).join("；");
    initializeButton.hidden = !setup.can_initialize;
    initializeButton.disabled = ["initializing", "restarting", "verifying", "rolling_back"].includes(setup.state);
    initializeButton.textContent = setup.requires_adb_passcode ? "自动解锁 ADB 并完成配置" : "初始化模块";
    installButton.hidden = Boolean(voice.runtime_installed);
  } catch (error) {
    summary.textContent = `语音准备状态读取失败：${error.message}`;
    detail.textContent = "";
    currentVoiceSetup = {};
    initializeButton.hidden = true;
    installButton.hidden = true;
  } finally {
    voiceSetupInFlight = false;
  }
}

function profileRows(value) {
  const groups = Array.isArray(value) ? value : value?.profiles || [];
  return groups.flatMap((group) =>
    (group.profiles || []).map((profile) => ({ ...profile, aid: group.aid_hex || "" })),
  );
}

function profileDisplayName(profile) {
  return profile?.name || profile?.service_provider_name || profile?.iccid || "未命名 Profile";
}

function activeProfile(profiles) {
  return profiles.find((profile) => profile.state === 1) || null;
}

function maskIdentifier(value, keep = 4) {
  const text = String(value || "");
  if (text.length <= keep * 2) return text;
  return `${text.slice(0, keep)} ${"•".repeat(Math.max(4, text.length - keep * 2))} ${text.slice(-keep)}`;
}

function maskPhoneNumber(value) {
  const text = String(value || "").trim();
  const digitCount = [...text].filter((char) => /\d/.test(char)).length;
  if (digitCount <= 8) return text;
  let digitIndex = 0;
  return [...text].map((char) => {
    if (!/\d/.test(char)) return char;
    digitIndex += 1;
    return digitIndex > 4 && digitIndex <= digitCount - 4 ? "*" : char;
  }).join("");
}

async function copyIdentifier(value, label) {
  try {
    await navigator.clipboard.writeText(value);
    notice(`${label} 已复制`);
  } catch (error) {
    notice(`复制 ${label} 失败，请手动复制`);
  }
}

async function editProfileNote(profile, note) {
  const values = await showModal({
    title: "编辑模块资料",
    message: "这些资料保存在大疆模块中，并按 ICCID 与当前 Profile 关联。",
    confirmLabel: "保存",
    fields: [
      { name: "label", label: "模块内名称", value: note.label || "", placeholder: "可选" },
      { name: "phone", label: "模块号码", value: note.phone || "", placeholder: "可选" },
      { name: "tags", label: "用途标签", value: note.tags || "", placeholder: "例如：英国验证码" },
    ],
  });
  if (!values) return;
  try {
    await api("/api/esim/module-notes", {
      method: "PUT",
      body: JSON.stringify({ iccid: profile.iccid, label: values.label, phone: values.phone, tags: values.tags }),
    });
    notice("模块资料已保存");
    await loadESIM();
  } catch (error) {
    notice(error.message);
  }
}

function phonebookCheck(label, ok, detail) {
  const card = document.createElement("div");
  card.className = `phonebook-check ${ok ? "ok" : ""}`;
  const title = document.createElement("strong");
  title.textContent = label;
  const text = document.createElement("small");
  text.textContent = detail;
  card.append(title, text);
  return card;
}

async function probeESIMPhonebook() {
  const button = $("#probe-esim-phonebook");
  const status = $("#esim-phonebook-status");
  const resultPanel = $("#esim-phonebook-result");
  button.disabled = true;
  status.textContent = "正在检测卡内通讯录能力，不会写入联系人...";
  resultPanel.hidden = true;
  try {
    const result = await api("/api/esim/phonebook/probe", { method: "POST" });
    const supported = result.storage_supported && result.storage_selected;
    const portable = supported && result.read_supported && result.write_supported;
    status.textContent = portable
      ? "已确认当前 Profile 支持卡内通讯录读写；尚未写入任何联系人。"
      : "当前 Profile 未完整确认卡内通讯录读写能力；不会进行写入。";
    resultPanel.replaceChildren(
      phonebookCheck("SIM 通讯录", result.storage_supported, result.storage_supported ? "支持 SM 卡内存储" : "未发现 SM 卡内存储"),
      phonebookCheck("当前卡片", result.storage_selected, result.storage_selected ? "已安全选中 SM 存储" : "无法选中 SM 存储"),
      phonebookCheck("读取能力", result.read_supported, result.read_supported ? "模块支持读取卡内联系人" : "模块未确认读取命令"),
      phonebookCheck("写入接口", result.write_supported, result.write_supported ? "模块声明支持写入接口" : "模块未确认写入命令"),
      phonebookCheck("当前状态", supported, result.storage_status || "未返回容量信息"),
    );
    resultPanel.hidden = false;
  } catch (error) {
    status.textContent = `通讯录检测失败：${error.message}`;
  } finally {
    button.disabled = false;
  }
}

function esimEIDRows(value) {
  const eids = value?.chip_info?.eids;
  return Array.isArray(eids) ? eids : [];
}

function renderESIMChip(overview) {
  const panel = $("#esim-chip");
  const chip = overview?.chip_info || {};
  const eids = esimEIDRows(overview);
  if (!chip.sku_name && !chip.serial_number && !chip.firmware && !eids.length) {
    panel.hidden = true;
    panel.replaceChildren();
    return;
  }
  panel.hidden = false;
  panel.replaceChildren(
    diagnosticCard("卡类型", chip.sku_name || "eUICC/eSIM 卡片"),
    diagnosticCard("固件", chip.firmware || "--", chip.serial_number ? `序列号 ${chip.serial_number}` : ""),
    diagnosticCard("EID", eids.map((item) => item.eid).filter(Boolean).join(" · ") || "--"),
  );
}

function renderESIMEIDList(overview) {
  const eids = esimEIDRows(overview);
  if (!eids.length) return [];
  return eids.map((item) => {
    const row = document.createElement("article");
    row.className = "item esim-info-row";
    const name = document.createElement("strong");
    name.textContent = "已识别 eUICC";
    const detail = document.createElement("p");
    detail.textContent = [
      item.eid ? `EID ${item.eid}` : "",
      item.aid ? `AID ${item.aid}` : "",
      item.free_nvram ? `可用空间 ${item.free_nvram}` : "",
      item.firmware ? `固件 ${item.firmware}` : "",
    ].filter(Boolean).join("\n");
    const status = document.createElement("small");
    status.textContent = item.spec || item.spec_guess || "eSIM";
    row.append(name, detail, status);
    return row;
  });
}

function renderESIMEIDPanel(rows) {
  if (!rows.length) return null;
  const panel = document.createElement("details");
  panel.className = "esim-euicc-panel";
  const heading = document.createElement("summary");
  heading.className = "esim-euicc-heading";
  const title = document.createElement("strong");
  title.textContent = "已识别 eUICC";
  const hint = document.createElement("small");
  hint.textContent = rows.length > 1 ? `${rows.length} 张 eSIM 卡片` : "卡片信息";
  heading.append(title, hint);
  panel.append(heading, ...rows);
  return panel;
}

async function loadESIMHealth() {
  if (esimHealthInFlight) return;
  esimHealthInFlight = true;
  const section = $("#esim-runtime-section");
  const panel = $("#esim-runtime");
  section.hidden = false;
  panel.replaceChildren(diagnosticCard("Profile 检查", "正在检测"));
  try {
    const health = await api("/api/esim/health");
    if (health.card_type === "physical_sim") {
      section.hidden = true;
      return;
    }
    if (!health.active_profile) {
      panel.replaceChildren(diagnosticCard("Profile 检查", health.message || "未发现已启用 Profile"));
      return;
    }
    const profile = health.active_profile;
    const signal = Number.isFinite(health.signal_dbm) ? `${health.signal_dbm} dBm` : "--";
    panel.replaceChildren(
      diagnosticCard("当前启用", profileDisplayName(profile), profile.iccid ? `ICCID ${maskIdentifier(profile.iccid)}` : ""),
      diagnosticCard("模块实际卡", health.module_iccid ? maskIdentifier(health.module_iccid) : "--", health.imsi ? `IMSI ${health.imsi}` : ""),
      diagnosticCard("蜂窝注册", health.registration || "未注册", [displayOperatorName(health.operator), health.network_mode].filter(Boolean).join(" · ")),
      diagnosticCard("信号", signal, health.registered ? "模块已接管当前 Profile" : "等待网络注册"),
    );
  } catch (error) {
    panel.replaceChildren(diagnosticCard("Profile 检查", "暂时无法读取", error.message));
  } finally {
    esimHealthInFlight = false;
  }
}

function setESIMHealthPolling(enabled) {
  clearInterval(esimHealthPollTimer);
  esimHealthPollTimer = null;
  if (!enabled) return;
  esimHealthPollTimer = setInterval(() => {
    if ($("#esim").classList.contains("active")) void loadESIMHealth();
  }, 30000);
}

function diagnosticCard(label, value, detail = "") {
  const card = document.createElement("div");
  card.className = "diagnostic-card";
  const span = document.createElement("span");
  span.textContent = label;
  const strong = document.createElement("strong");
  strong.textContent = value || "--";
  card.append(span, strong);
  if (detail) {
    const small = document.createElement("small");
    small.textContent = detail;
    card.append(small);
  }
  return card;
}

function renderNetworkCheck(label, result) {
  const list = $("#network-checks");
  list.className = "list";
  const row = document.createElement("article");
  row.className = `item check-item ${result.ok ? "ok" : "bad"}`;
  const name = document.createElement("strong");
  name.textContent = label;
  const detail = document.createElement("p");
  detail.textContent = result.detail || result.summary || "";
  const status = document.createElement("small");
  status.textContent = result.ok ? "通过" : "未通过";
  row.append(name, detail, status);
  const existing = [...list.querySelectorAll(".item")].filter((item) => item.dataset.label !== label);
  row.dataset.label = label;
  list.replaceChildren(row, ...existing);
}

async function runNetworkCheck(label, path, button) {
  button.disabled = true;
  try {
    const result = await api(path, { method: "POST" });
    renderNetworkCheck(label, result);
    notice(result.summary || "检测完成");
  } catch (error) {
    renderNetworkCheck(label, { ok: false, summary: "检测失败", detail: error.message });
    notice(error.message);
  } finally {
    button.disabled = false;
  }
}

async function loadNetwork() {
  const grid = $("#network-grid");
  const ifaceList = $("#network-interfaces");
  $("#network-status").textContent = "正在读取网络诊断...";
  try {
    const diag = await api("/api/network");
    const active = Array.isArray(diag.active_contexts) ? diag.active_contexts.join(", ") : "";
    const apns = Array.isArray(diag.pdp_contexts)
      ? diag.pdp_contexts.map((ctx) => `${ctx.id}:${ctx.apn}`).join(" · ")
      : "";
    const addresses = Array.isArray(diag.pdp_addresses) ? diag.pdp_addresses.join(" · ") : "";
    const usb = diag.usb_device
      ? `${diag.usb_device.vendor || ""} ${diag.usb_device.product || ""} (${diag.usb_device.vendor_id}:${diag.usb_device.product_id})`
      : "未检测到";
    const route = diag.default_route || {};
    const routeText = route.interface
      ? `${route.interface}${route.gateway ? ` -> ${route.gateway}` : ""}`
      : "未知";
    grid.replaceChildren(
      diagnosticCard("USB 网卡", diag.usb_network_present ? "已识别" : "未识别", "macOS 是否出现可用 USB 网络接口"),
      diagnosticCard("默认出口", routeText, "当前 macOS 实际优先使用的网卡和网关"),
      diagnosticCard("usbnet", diag.usbnet_mode || "未知", "模块当前 USB 网络模式"),
      diagnosticCard("蜂窝数据", active ? `已激活 ${active}` : "未激活", "PDP context 激活状态"),
      diagnosticCard("蜂窝 IP", addresses || "无", "模块侧拿到的数据网络地址"),
      diagnosticCard("APN", apns || "无", "当前可见 PDP 配置"),
      diagnosticCard("USB 枚举", usb, diag.usb_device?.mode || ""),
    );

    const errorText = diag.errors ? ` · 错误：${Object.values(diag.errors).join("；")}` : "";
    $("#network-status").textContent = diag.usb_network_present
      ? `macOS 已识别 USB 网络接口${errorText}`
      : `蜂窝侧可能已通，但 macOS 尚未识别 USB 网卡${errorText}`;

    const interfaces = Array.isArray(diag.mac_interfaces) ? diag.mac_interfaces : [];
    if (!interfaces.length) {
      ifaceList.className = "list empty";
      ifaceList.textContent = "未读取到网络接口";
      return;
    }
    ifaceList.className = "list";
    ifaceList.replaceChildren(...interfaces.map((item) => {
      const row = document.createElement("article");
      row.className = "item";
      const name = document.createElement("strong");
      name.textContent = item.name;
      const detail = document.createElement("p");
      detail.textContent = [item.kind, item.status, item.ipv4].filter(Boolean).join(" · ");
      const status = document.createElement("small");
      status.textContent = item.status === "active" ? "active" : "inactive";
      row.append(name, detail, status);
      return row;
    }));
  } catch (error) {
    $("#network-status").textContent = `读取网络诊断失败：${error.message}`;
    grid.replaceChildren();
    ifaceList.className = "list empty";
    ifaceList.textContent = "读取失败";
    notice(error.message);
  }
}

function formatTrafficBytes(value) {
  const bytes = Math.max(0, Number(value || 0));
  const units = ["B", "KB", "MB", "GB", "TB"];
  let amount = bytes;
  let unit = 0;
  while (amount >= 1024 && unit < units.length - 1) {
    amount /= 1024;
    unit += 1;
  }
  const digits = unit === 0 ? 0 : (amount >= 100 ? 0 : amount >= 10 ? 1 : 2);
  return `${amount.toFixed(digits)} ${units[unit]}`;
}

async function loadNetworkTraffic() {
  if (networkTrafficInFlight) return;
  networkTrafficInFlight = true;
  try {
    const sample = await api("/api/network/traffic");
    if (!sample.available) {
      networkTrafficPrevious = null;
      setValue("#traffic-rx-rate", "--", "muted");
      setValue("#traffic-tx-rate", "--", "muted");
      setValue("#traffic-session-rx", "--", "muted");
      setValue("#traffic-session-tx", "--", "muted");
      setValue("#traffic-session-total", "--", "muted");
      return;
    }

    let rxRate = 0;
    let txRate = 0;
    const previous = networkTrafficPrevious;
    if (previous && previous.interface === sample.interface) {
      const elapsed = (Number(sample.sampled_at_ms) - Number(previous.sampled_at_ms)) / 1000;
      if (elapsed > 0) {
        rxRate = Math.max(0, Number(sample.rx_bytes) - Number(previous.rx_bytes)) / elapsed;
        txRate = Math.max(0, Number(sample.tx_bytes) - Number(previous.tx_bytes)) / elapsed;
      }
    }
    networkTrafficPrevious = sample;
    setValue("#traffic-rx-rate", `${formatTrafficBytes(rxRate)}/s`, "neutral");
    setValue("#traffic-tx-rate", `${formatTrafficBytes(txRate)}/s`, "neutral");
    setValue("#traffic-session-rx", formatTrafficBytes(sample.session_rx_bytes), "neutral");
    setValue("#traffic-session-tx", formatTrafficBytes(sample.session_tx_bytes), "neutral");
    setValue("#traffic-session-total", formatTrafficBytes(sample.session_total_bytes), "emphasis");
    $("#traffic-session-total").title = "本次启动期间的下载与上传流量之和；关闭 DJOneHub 后清零";
  } catch (error) {
    setValue("#traffic-rx-rate", "--", "muted");
    setValue("#traffic-tx-rate", "--", "muted");
    setValue("#traffic-session-total", "--", "muted");
  } finally {
    networkTrafficInFlight = false;
  }
}

function setNetworkTrafficPolling(enabled) {
  clearInterval(networkTrafficTimer);
  networkTrafficTimer = null;
  if (!enabled) {
    networkTrafficPrevious = null;
    return;
  }
  void loadNetworkTraffic();
  networkTrafficTimer = setInterval(loadNetworkTraffic, 1000);
}

async function setUSBNetMode(mode) {
  const label = `模式 ${mode}`;
  const confirmed = await showModal({
    title: `切换到${label}`,
    message: `将写入 usbnet=${mode}，重启模块后生效。`,
    confirmLabel: "继续切换",
  });
  if (!confirmed) return;
  try {
    const result = await api("/api/network/usbnet", {
      method: "POST",
      body: JSON.stringify({ mode }),
    });
    notice(`usbnet 已写入 ${result.mode}，请重启模块`);
    await loadNetwork();
  } catch (error) {
    notice(error.message);
  }
}

async function switchWorkMode(mode, label, button) {
  const confirmed = await showModal({
    title: `切换到${label}`,
    message: `将写入 usbnet=${mode} 并重启模块，USB 会短暂断开。`,
    confirmLabel: "确认切换",
  });
  if (!confirmed) return;
  const status = $("#workmode-status");
  const buttons = [$("#workmode-sms"), $("#workmode-network")];
  buttons.forEach((item) => { item.disabled = true; });
  status.hidden = false;
  status.textContent = `正在切到${label}...`;
  try {
    const result = await api("/api/network/usbnet", {
      method: "POST",
      body: JSON.stringify({ mode }),
    });
    status.textContent = `usbnet 已写入 ${result.mode}，正在重启模块...`;
    await api("/api/network/reboot-module", { method: "POST" });
    status.textContent = `${label}已写入，等待模块重新枚举后自动刷新。`;
    notice(`${label}切换中`);
    setTimeout(loadStatus, 8000);
    setTimeout(loadNetwork, 12000);
    setTimeout(() => {
      status.textContent = `${label}切换完成后，请确认状态卡和网络诊断。`;
      buttons.forEach((item) => { item.disabled = false; });
    }, 13000);
  } catch (error) {
    status.hidden = false;
    status.textContent = `${label}切换失败：${error.message}`;
    notice(error.message);
    buttons.forEach((item) => { item.disabled = false; });
  }
}

async function rebootModule() {
  const confirmed = await showModal({
    title: "重启模块",
    message: "模块会重新枚举 USB，网页可能短暂断开。",
    confirmLabel: "确认重启",
  });
  if (!confirmed) return;
  try {
    await api("/api/network/reboot-module", { method: "POST" });
    notice("模块正在重启，稍后刷新状态");
    setTimeout(loadStatus, 8000);
    setTimeout(loadNetwork, 12000);
  } catch (error) {
    notice(error.message);
  }
}

async function loadESIM() {
  const list = $("#esim-list");
  const status = $("#esim-status");
  const download = $("#esim-download-section");
  const runtime = $("#esim-runtime-section");
  const profilePanel = $("#esim-profile-panel");
  const phonebook = $("#esim-phonebook-section");
  $("#esim-chip").hidden = true;
  $("#esim-chip").replaceChildren();
  runtime.hidden = true;
  download.hidden = false;
  profilePanel.hidden = false;
  phonebook.hidden = false;
  list.className = "list empty";
  list.textContent = "正在读取 eUICC";
  status.textContent = "正在通过 AT+CCHO/CGLA 读取 eUICC/eSIM 卡片";
  try {
    const overview = await api("/api/esim");
    if (overview.card_type === "physical_sim") {
      status.textContent = overview.message;
      list.textContent = overview.message;
      download.hidden = true;
      profilePanel.hidden = true;
      phonebook.hidden = true;
      setESIMHealthPolling(false);
      return;
    }
    const notesResponse = await api("/api/esim/module-notes");
    const notes = notesResponse.notes || {};
    const profiles = profileRows(overview);
    const eidRows = renderESIMEIDList(overview);
    const eidPanel = renderESIMEIDPanel(eidRows);
    renderESIMChip(overview);
    const profileCount = profiles.length;
    const eidCount = esimEIDRows(overview).length;
    const active = activeProfile(profiles);
    status.textContent = active
      ? `已读取：${eidCount} 个 eUICC，${profileCount} 个 Profile · 当前使用 ${profileDisplayName(active)}`
      : `已读取：${eidCount} 个 eUICC，${profileCount} 个 Profile · 未发现已启用 Profile`;
    if (!profiles.length) {
      if (eidRows.length) {
        list.className = "list";
        list.replaceChildren(eidPanel);
        return;
      }
      list.textContent = "未发现 eUICC/eSIM 卡片参数";
      return;
    }
    list.className = "list";
    const profileItems = profiles.map((profile) => {
      const note = notes[profile.iccid] || {};
      const row = document.createElement("article");
      row.className = `item esim-profile ${profile.state === 1 ? "active" : ""}`;
      const name = document.createElement("strong");
      name.textContent = note.label || profileDisplayName(profile);
      const detail = document.createElement("p");
      detail.textContent = [
        note.label && note.label !== profileDisplayName(profile) ? `卡内名称：${profileDisplayName(profile)}` : "",
        profile.service_provider_name ? `服务商：${profile.service_provider_name}` : "",
        profile.class_text ? `类型：${profile.class_text}` : "",
        note.tags ? `标签：${note.tags}` : "",
      ].filter(Boolean).join("\n");
      const metadata = document.createElement("div");
      metadata.className = "profile-metadata";
      if (note.phone) {
        const phoneRow = document.createElement("div");
        phoneRow.className = "profile-identifier-row";
        const phone = document.createElement("code");
        phone.className = "profile-iccid";
        phone.textContent = `模块号码 ${maskPhoneNumber(note.phone)}`;
        const revealPhone = document.createElement("button");
        revealPhone.className = "secondary compact profile-toggle-button";
        revealPhone.type = "button";
        revealPhone.textContent = "显示";
        revealPhone.addEventListener("click", () => {
          const hidden = revealPhone.textContent === "显示";
          phone.textContent = `模块号码 ${hidden ? note.phone : maskPhoneNumber(note.phone)}`;
          revealPhone.textContent = hidden ? "隐藏" : "显示";
        });
        const copyPhone = document.createElement("button");
        copyPhone.className = "secondary compact profile-copy-button";
        copyPhone.type = "button";
        copyPhone.textContent = "复制号码";
        copyPhone.addEventListener("click", () => copyIdentifier(note.phone, "模块号码"));
        phoneRow.append(phone, revealPhone, copyPhone);
        metadata.append(phoneRow);
      }
      if (profile.iccid) {
        const iccidRow = document.createElement("div");
        iccidRow.className = "profile-identifier-row";
        const iccid = document.createElement("code");
        iccid.className = "profile-iccid";
        iccid.textContent = `ICCID ${maskIdentifier(profile.iccid)}`;
        const reveal = document.createElement("button");
        reveal.className = "secondary compact profile-toggle-button";
        reveal.type = "button";
        reveal.textContent = "显示";
        reveal.addEventListener("click", () => {
          const hidden = reveal.textContent === "显示";
          iccid.textContent = `ICCID ${hidden ? profile.iccid : maskIdentifier(profile.iccid)}`;
          reveal.textContent = hidden ? "隐藏" : "显示";
        });
        const copy = document.createElement("button");
        copy.className = "secondary compact profile-copy-button";
        copy.type = "button";
        copy.textContent = "复制 ICCID";
        copy.addEventListener("click", () => copyIdentifier(profile.iccid, "ICCID"));
        iccidRow.append(iccid, reveal, copy);
        metadata.append(iccidRow);
      }
      const actionBox = document.createElement("div");
      actionBox.className = "profile-actions";
      if (profile.state !== 1) {
        const button = document.createElement("button");
        button.className = "compact";
        button.textContent = "启用";
        button.addEventListener("click", async () => {
          const label = profileDisplayName(profile);
          const confirmed = await showModal({
            title: "启用 Profile",
            message: `确定启用 ${label} 吗？当前正在使用的 eSIM Profile 会被切换。`,
            confirmLabel: "启用",
          });
          if (!confirmed) {
            return;
          }
          button.disabled = true;
          button.textContent = "切换中";
          try {
            const result = await api("/api/esim/switch", {
              method: "POST",
              body: JSON.stringify({ iccid: profile.iccid, aid: profile.aid || "" }),
            });
            if (result.module_reboot_requested) {
              status.textContent = `已切换到 ${label}；模块正在重启，等待新 Profile 接管（约 ${result.reconnect_wait_seconds || 10} 秒）`;
              notice(`已切换 ${label}，模块正在重新读取新卡`);
              setTimeout(async () => {
                await loadESIM();
                await loadStatus();
              }, (result.reconnect_wait_seconds || 10) * 1000);
            } else {
              status.textContent = `Profile 已切换到 ${label}，但模块重启未确认：${result.module_reboot_warning || "请手动重启后再读取号码"}`;
              notice("Profile 已切换，模块重启未确认");
              await loadESIM();
            }
          } catch (error) {
            status.textContent = `切换失败：${error.message}`;
            notice(error.message);
            button.disabled = false;
            button.textContent = "启用";
          }
        });
        actionBox.append(button);
      } else {
        const button = document.createElement("button");
        button.className = "secondary compact";
        button.type = "button";
        button.textContent = "启用";
        button.disabled = true;
        actionBox.append(button);
      }
      const rename = document.createElement("button");
      rename.className = "secondary compact";
      rename.type = "button";
      rename.textContent = "改名";
      rename.addEventListener("click", async () => {
        const values = await showModal({
          title: "修改 Profile 名称",
          message: "名称将写入 eUICC 卡片内部的 Profile nickname。",
          confirmLabel: "保存",
          fields: [{ name: "name", label: "Profile 名称", value: profileDisplayName(profile), required: true }],
        });
        if (!values?.name) return;
        rename.disabled = true;
        try {
          await api("/api/esim/profile", { method: "PATCH", body: JSON.stringify({ iccid: profile.iccid, aid: profile.aid || "", name: values.name }) });
          notice("Profile 名称已修改");
          await loadESIM();
        } catch (error) { notice(error.message); } finally { rename.disabled = false; }
      });
      const localNote = document.createElement("button");
      localNote.className = "secondary compact";
      localNote.type = "button";
      localNote.textContent = "模块资料";
      localNote.addEventListener("click", () => editProfileNote(profile, note));
      const remove = document.createElement("button");
      remove.className = "secondary danger compact";
      remove.type = "button";
      remove.textContent = "删除";
      remove.disabled = profile.state === 1;
      remove.addEventListener("click", async () => {
        const last4 = String(profile.iccid || "").slice(-4);
        const values = await showModal({
          title: "删除 Profile",
          message: `删除不可恢复。请输入 ICCID 后四位 ${last4} 确认。`,
          confirmLabel: "删除",
          danger: true,
          fields: [{ name: "confirmation", label: "ICCID 后四位", required: true }],
        });
        if (!values) return;
        if (values.confirmation !== last4) {
          notice("ICCID 后四位不匹配，未执行删除");
          return;
        }
        remove.disabled = true;
        try {
          await api("/api/esim/profile", { method: "DELETE", body: JSON.stringify({ iccid: profile.iccid, aid: profile.aid || "" }) });
          notice("Profile 已删除");
          await loadESIM();
        } catch (error) { notice(error.message); } finally { remove.disabled = false; }
      });
      actionBox.append(localNote, rename, remove);
      const description = document.createElement("div");
      description.className = "profile-description";
      description.append(detail, metadata);
      row.append(name, description, actionBox);
      return row;
    });
    list.replaceChildren(...(eidPanel ? [eidPanel] : []), ...profileItems);
    void loadESIMHealth();
    setESIMHealthPolling(true);
  } catch (error) {
    status.textContent = `读取失败：${error.message}`;
    list.textContent = error.message;
    setESIMHealthPolling(false);
  }
}

document.querySelectorAll(".tab").forEach((tab) => {
  tab.addEventListener("click", () => {
    document.querySelectorAll(".tab, .view").forEach((el) => el.classList.remove("active"));
    tab.classList.add("active");
    $(`#${tab.dataset.view}`).classList.add("active");
    if (tab.dataset.view === "esim") loadESIM();
    else setESIMHealthPolling(false);
    if (tab.dataset.view === "network") loadNetwork();
    if (tab.dataset.view === "calls") {
      loadCalls();
      loadBarkSettings("call");
      loadVoiceAgentCalls();
      connectVoiceAgentEvents();
    } else {
      closeVoiceAgentEvents();
    }
  });
});

$("#esim-download-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  // currentTarget is only populated while the submit event is being handled;
  // retain the button before awaiting the confirmation dialog.
  const button = event.submitter || event.currentTarget.querySelector("button[type=submit]");
  const confirmed = await showModal({
    title: "下载新的 Profile",
    message: "将向 SM-DP+ 服务器下载并写入新的 eSIM Profile。写入期间请勿拔出模块。",
    confirmLabel: "开始下载",
  });
  if (!confirmed) return;
  const status = $("#esim-download-status");
  button.disabled = true;
  status.textContent = "正在下载并写入 Profile，请勿拔出模块...";
  try {
    const result = await api("/api/esim/download", { method: "POST", body: JSON.stringify({
      smdp: $("#esim-smdp").value, matching_id: $("#esim-matching-id").value,
      confirmation_code: $("#esim-confirmation-code").value, imei: $("#esim-imei").value, aid: $("#esim-aid").value,
    }) });
    status.textContent = result.message || "Profile 下载完成，正在重新读取卡片";
    notice("Profile 下载完成");
    await loadESIM();
  } catch (error) { status.textContent = `下载失败：${error.message}`; notice(error.message); } finally { button.disabled = false; }
});

$("#send-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const button = event.submitter;
  const originalLabel = button.textContent;
  button.disabled = true;
  button.textContent = "发送中";
  try {
    const result = await api("/api/sms/send", {
      method: "POST",
      body: JSON.stringify({ phone: $("#phone").value, message: $("#message").value }),
    });
    $("#message").value = "";
    const segments = Number(result.segments || 1);
    notice(segments > 1 ? `短信已发送（${segments} 个分片）` : "短信已发送");
  } catch (error) {
    notice(error.message);
  } finally {
    button.disabled = false;
    button.textContent = originalLabel;
  }
});

$("#sms-bark-settings-form").addEventListener("submit", (event) => saveBarkSettings(event, "sms"));
$("#test-sms-bark").addEventListener("click", () => testBarkSettings("sms"));
$("#call-bark-settings-form").addEventListener("submit", (event) => saveBarkSettings(event, "call"));
$("#test-call-bark").addEventListener("click", () => testBarkSettings("call"));
$("#refresh-calls").addEventListener("click", loadCalls);
$("#refresh-call-history").addEventListener("click", loadCalls);

$("#voice-agent-form").addEventListener("input", (event) => {
  voiceAgentFormDirty = true;
  if (event.target.id === "voice-agent-provider" || event.target.id === "voice-agent-fallback-provider") {
    if (event.target.id === "voice-agent-provider") normalizeVoiceAgentVoiceForProvider();
    updateVoiceAgentFieldVisibility();
  }
});
$("#voice-agent-form").addEventListener("change", (event) => {
  voiceAgentFormDirty = true;
  if (event.target.id === "voice-agent-provider" || event.target.id === "voice-agent-fallback-provider") {
    if (event.target.id === "voice-agent-provider") normalizeVoiceAgentVoiceForProvider();
    updateVoiceAgentFieldVisibility();
  }
});
$("#voice-agent-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  await saveVoiceAgentProfile();
});
$("#voice-agent-manual").addEventListener("click", async () => {
  $("#voice-agent-enabled").checked = false;
  await saveVoiceAgentProfile({ forceDisabled: true });
});
$("#clear-live-transcript").addEventListener("click", () => {
  liveVoiceAgentEvents = [];
  renderVoiceAgentEvents($("#voice-agent-transcript"), [], "等待 Voice Agent 事件...");
});
$("#refresh-voice-agent-calls").addEventListener("click", loadVoiceAgentCalls);
$("#back-to-voice-agent-calls").addEventListener("click", () => {
  closeVoiceAgentCallDetail();
  $("#voice-agent-call-panel").open = true;
  $("#voice-agent-call-panel").scrollIntoView({ behavior: "smooth", block: "start" });
});

$("#initialize-voice-module").addEventListener("click", async () => {
  const requiresADBUnlock = Boolean(currentVoiceSetup.requires_adb_passcode);
  const confirmed = await showModal({
    title: requiresADBUnlock ? "自动解锁模块 ADB" : "初始化模块语音支持",
    message: requiresADBUnlock
      ? `将根据模块 challenge ${currentVoiceSetup.adb_challenge || "未知"} 自动生成并提交 QADBKEY passcode，再启用 ADB、USB Audio、IMS 与 VoLTE。passcode 不会保存或显示在日志中。模块随后会重启。`
      : "将先按当前 Device ID 备份 USB、IMS 与 VoLTE 配置，再写入并回读验证。模块会重启；失败时自动恢复全部原始配置。",
    confirmLabel: requiresADBUnlock ? "确认自动解锁并重启" : "确认备份并初始化",
    danger: true,
  });
  if (!confirmed) return;
  const button = $("#initialize-voice-module");
  button.disabled = true;
  try {
    await api("/api/module/setup", {
      method: "POST",
      body: JSON.stringify({ confirm: true }),
    });
    notice("初始化已开始，正在等待模块重新连接");
    await loadVoiceSetup();
  } catch (error) {
    notice(error.message);
  } finally {
    button.disabled = false;
  }
});

$("#install-voice-runtime").addEventListener("click", async () => {
  const confirmed = await showModal({
    title: "安装模块侧语音运行时",
    message: "将从固定的 MaVo 上游提交下载三个模块文件。每个文件必须通过内置 SHA-256 校验才会保存，不会写入未校验内容。",
    confirmLabel: "确认下载并校验",
  });
  if (!confirmed) return;
  const button = $("#install-voice-runtime");
  button.disabled = true;
  try {
    await api("/api/voice/provision", { method: "POST", body: JSON.stringify({ confirm: true }) });
    notice("语音运行时已下载并校验");
    await loadVoiceSetup();
  } catch (error) {
    notice(error.message);
  } finally {
    button.disabled = false;
  }
});

async function runCallAction(button, path, successMessage, body) {
  const originalLabel = button.textContent;
  button.disabled = true;
  try {
    await api(path, {
      method: "POST",
      ...(body === undefined ? {} : { body: JSON.stringify(body) }),
    });
    notice(successMessage);
    await loadCalls();
  } catch (error) {
    notice(error.message);
  } finally {
    button.disabled = button.id === "dial-call" && currentCallState !== null;
    button.textContent = originalLabel;
  }
}

$("#dial-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const number = $("#dial-number").value.trim();
  if (!number) {
    notice("请输入电话号码");
    return;
  }
  const button = $("#dial-call");
  await runCallAction(button, "/api/calls/dial", `正在拨打 ${number}`, { number });
  if (currentCallState) $("#dial-number").value = "";
});

$("#answer-call").addEventListener("click", () =>
  runCallAction($("#answer-call"), "/api/calls/answer", "已发送接听命令"));
$("#reject-call").addEventListener("click", () =>
  runCallAction($("#reject-call"), "/api/calls/reject", "已拒接来电"));
$("#hangup-call").addEventListener("click", () =>
  runCallAction($("#hangup-call"), "/api/calls/hangup", "已发送挂断命令"));

$("#mute-call").addEventListener("click", async () => {
  await runCallAction($("#mute-call"), "/api/calls/audio/mute", currentAudioHostState.want_muted ? "已取消静音" : "已静音", {
    muted: !currentAudioHostState.want_muted,
  });
});

$("#record-call").addEventListener("click", async () => {
  await runCallAction($("#record-call"), "/api/calls/audio/record", currentAudioHostState.want_recording ? "录音已停止" : "录音已开始", {
    enabled: !currentAudioHostState.want_recording,
  });
});

$("#dtmf-pad").addEventListener("click", async (event) => {
  const button = event.target.closest("[data-dtmf]");
  if (!button) return;
  await runCallAction(button, "/api/calls/dtmf", `已发送按键 ${button.dataset.dtmf}`, {
    digit: button.dataset.dtmf,
  });
});

$("#at-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const output = $("#at-output");
  output.textContent = "执行中";
  try {
    const result = await api("/api/at", {
      method: "POST",
      body: JSON.stringify({ command: $("#at-command").value }),
    });
    output.textContent = result.response || "OK";
  } catch (error) {
    output.textContent = error.message;
  }
});

$("#refresh").addEventListener("click", async () => {
  const tasks = [loadStatus(), loadSMS()];
  if ($("#calls").classList.contains("active")) tasks.push(loadCalls());
  await Promise.all(tasks);
  notice("状态已刷新");
});
$("#refresh-sms").addEventListener("click", async () => {
  const button = $("#refresh-sms");
  button.disabled = true;
  $("#sms-status").textContent = "正在读取短信...";
  try {
    const result = await api("/api/sms/refresh", { method: "POST" });
    await loadSMS();
    $("#sms-status").textContent = `短信读取完成：${result.count ?? "未知"} 条`;
    notice("短信读取完成");
  } catch (error) {
    $("#sms-status").textContent = `读取短信失败：${error.message}`;
    notice(error.message);
  } finally {
    button.disabled = false;
  }
});
$("#clear-module-sms").addEventListener("click", async () => {
  const confirmed = await showModal({
    title: "清空模块旧短信",
    message: "只会清空模块内部 ME 存储里的旧短信，不会删除 SIM 卡短信。",
    confirmLabel: "确认清空",
    danger: true,
  });
  if (!confirmed) return;
  const button = $("#clear-module-sms");
  button.disabled = true;
  $("#sms-status").textContent = "正在清空模块内部旧短信...";
  try {
    const result = await api("/api/sms/clear-module", { method: "POST" });
    $("#sms-status").textContent = `模块旧短信已清理：${result.before ?? 0} -> ${result.after ?? 0} 条`;
    await loadSMS();
    notice("模块旧短信已清理");
  } catch (error) {
    $("#sms-status").textContent = `清理模块旧短信失败：${error.message}`;
    notice(error.message);
  } finally {
    button.disabled = false;
  }
});
$("#refresh-esim").addEventListener("click", loadESIM);
$("#probe-esim-phonebook").addEventListener("click", probeESIMPhonebook);
$("#refresh-network").addEventListener("click", loadNetwork);
$("#workmode-sms").addEventListener("click", () =>
  switchWorkMode(0, "短信模式", $("#workmode-sms")));
$("#workmode-network").addEventListener("click", () =>
  switchWorkMode(1, "上网模式", $("#workmode-network")));
$("#check-4g-route").addEventListener("click", () =>
  runNetworkCheck("4G 出口", "/api/network/check-4g", $("#check-4g-route")));
$("#check-proxy-route").addEventListener("click", () =>
  runNetworkCheck("代理", "/api/network/check-proxy", $("#check-proxy-route")));
$("#usbnet-mode-0").addEventListener("click", () => setUSBNetMode(0));
$("#usbnet-mode-1").addEventListener("click", () => setUSBNetMode(1));
$("#usbnet-mode-2").addEventListener("click", () => setUSBNetMode(2));
$("#usbnet-mode-3").addEventListener("click", () => setUSBNetMode(3));
$("#reboot-module").addEventListener("click", rebootModule);

$("#device-select").addEventListener("change", async (event) => {
  const next = event.currentTarget.value;
  if (!next || next === activeDeviceID) return;
  activeDeviceID = next;
  localStorage.setItem("djonehub-active-device", activeDeviceID);
  await refreshActiveDevice();
  const device = knownDevices.find((item) => item.id === activeDeviceID);
  notice(`已切换到${device?.alias ? ` ${device.alias}` : "所选模块"}`);
});

$("#rename-device").addEventListener("click", async () => {
  const device = knownDevices.find((item) => item.id === activeDeviceID);
  if (!device) return;
  const values = await showModal({
    title: "重命名模块",
    message: "名称会保存在本机，用于区分不同模块和 SIM。",
    confirmLabel: "保存名称",
    fields: [{ name: "alias", label: "模块名称", value: device.alias || "", required: true, maxLength: 80 }],
  });
  if (!values?.alias) return;
  try {
    await api(`/api/devices/${encodeURIComponent(activeDeviceID)}`, {
      method: "PATCH",
      body: JSON.stringify({ alias: values.alias }),
    });
    await loadDevices();
    notice("模块名称已保存");
  } catch (error) {
    notice(error.message);
  }
});

async function bootstrap() {
  await loadDevices({ refreshOnChange: false });
  await refreshActiveDevice();
  setNetworkTrafficPolling(true);
}

void bootstrap();
setInterval(loadDevices, 4000);
setInterval(loadStatus, 10000);
setInterval(loadSMS, 5000);
setInterval(() => {
  if ($("#calls").classList.contains("active")) loadCalls();
}, 2000);
