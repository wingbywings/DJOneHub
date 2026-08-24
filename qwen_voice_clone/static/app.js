const form = document.querySelector('#clone-form');
const input = document.querySelector('#audio');
const label = document.querySelector('#file-label');
const preview = document.querySelector('#audio-preview');
const message = document.querySelector('#message');
const button = document.querySelector('#submit-button');
const list = document.querySelector('#voice-list');
const empty = document.querySelector('#empty-state');

function escapeHtml(value) {
  const node = document.createElement('div');
  node.textContent = value ?? '';
  return node.innerHTML;
}

function showMessage(text, type) {
  message.hidden = false;
  message.className = `message ${type}`;
  message.textContent = text;
}

function selectFile(file) {
  if (!file) return;
  label.textContent = file.name;
  preview.src = URL.createObjectURL(file);
  preview.hidden = false;
  preview.onloadedmetadata = () => {
    if (preview.duration > 60) showMessage('音频超过 60 秒，请重新选择。', 'error');
  };
}

input.addEventListener('change', () => selectFile(input.files[0]));
const dropZone = document.querySelector('#drop-zone');
['dragenter', 'dragover'].forEach((event) => dropZone.addEventListener(event, (e) => {
  e.preventDefault(); dropZone.classList.add('dragging');
}));
['dragleave', 'drop'].forEach((event) => dropZone.addEventListener(event, (e) => {
  e.preventDefault(); dropZone.classList.remove('dragging');
}));
dropZone.addEventListener('drop', (event) => {
  const file = event.dataTransfer.files[0];
  if (!file) return;
  const transfer = new DataTransfer(); transfer.items.add(file); input.files = transfer.files;
  selectFile(file);
});

function renderVoices(voices) {
  empty.hidden = voices.length > 0;
  list.innerHTML = voices.map((voice) => `
    <article class="voice-card">
      <div class="voice-main">
        <span class="voice-prefix">${escapeHtml(voice.prefix)}</span>
        <code>${escapeHtml(voice.voice_id)}</code>
        <p>${escapeHtml(voice.target_model)} · ${escapeHtml(voice.original_filename)}</p>
      </div>
      <div class="voice-actions">
        <time>${new Date(voice.created_at).toLocaleString('zh-CN')}</time>
        <button class="copy-button" data-id="${escapeHtml(voice.voice_id)}">复制 ID</button>
      </div>
    </article>`).join('');
  document.querySelectorAll('.copy-button').forEach((copyButton) => {
    copyButton.addEventListener('click', async () => {
      await navigator.clipboard.writeText(copyButton.dataset.id);
      copyButton.textContent = '已复制';
      setTimeout(() => { copyButton.textContent = '复制 ID'; }, 1200);
    });
  });
}

async function loadVoices() {
  const response = await fetch('/api/voices');
  const body = await response.json();
  renderVoices(body.voices || []);
}

form.addEventListener('submit', async (event) => {
  event.preventDefault();
  if (preview.duration > 60) return showMessage('音频超过 60 秒，请重新选择。', 'error');
  button.disabled = true;
  button.querySelector('span').textContent = '上传并生成中…';
  message.hidden = true;
  try {
    const response = await fetch('/api/voices', { method: 'POST', body: new FormData(form) });
    const body = await response.json();
    if (!response.ok) throw new Error(body.error || '创建音色失败');
    showMessage(`创建成功：${body.voice.voice_id}`, 'success');
    await loadVoices();
  } catch (error) {
    showMessage(error.message, 'error');
  } finally {
    button.disabled = false;
    button.querySelector('span').textContent = '生成音色 ID';
  }
});

loadVoices().catch(() => showMessage('读取音色历史失败。', 'error'));

