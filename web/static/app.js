// The UI is htmx-driven; this is only for the three things htmx has no verb
// for — copying a value between fields and clearing a container.

function pickRecent(sel) {
  if (!sel.value) return;
  document.getElementById('dir').value = sel.value;
  sel.value = '';
}

function useFolder() {
  const cur = document.getElementById('browser-path');
  if (cur) document.getElementById('dir').value = cur.textContent.trim();
  closeBrowser();
}

function closeBrowser() {
  const b = document.getElementById('browser');
  if (b) b.innerHTML = '';
}
