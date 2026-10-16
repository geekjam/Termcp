/* Workspace notices share the global module scope, as the other UI modules do. */
var storageNoticeBanner = document.getElementById('storage-cleanup-banner');
var storageNoticeCode = '';
function renderStorageNotice() {
  storageNoticeBanner.style.display = storageNoticeCode ? '' : 'none';
  if (storageNoticeCode === 'cleanup') storageNoticeBanner.textContent = t('storage.cleanupPending');
  else if (storageNoticeCode === 'uncertain') storageNoticeBanner.textContent = t('storage.error.keySaveUncertain');
  else storageNoticeBanner.textContent = '';
}
fetch(apiPath('/api/storage/status'), { credentials: 'same-origin', cache: 'no-store' }).then(function (response) {
  return response.ok ? response.json() : null;
}).then(function (status) {
  if (status && status.state === 'CLEANUP_PENDING') storageNoticeCode = 'cleanup';
  else if (status && status.error_code === 'key_save_uncertain') storageNoticeCode = 'uncertain';
  renderStorageNotice();
}).catch(function () {});
onLangChange(renderStorageNotice);
