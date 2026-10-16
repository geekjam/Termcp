/* Password values stay in this form and request bodies only. */
(function () {
  'use strict';
  var form = document.getElementById('storage-form');
  var current = document.getElementById('storage-current');
  var next = document.getElementById('storage-new');
  var confirm = document.getElementById('storage-confirm');
  var submit = document.getElementById('storage-submit');
  var error = document.getElementById('storage-error');
  var status = null, busy = false, initializedHere = false, lastErrorCode = '';
  var language = document.getElementById('storage-language');
  language.value = storedLangChoice();
  language.addEventListener('change', function () { chooseLang(language.value); render(); });
  function ready() { return status && (status.state === 'READY' || status.state === 'CLEANUP_PENDING'); }
  function showError(code) {
    lastErrorCode = code;
    var messages = {
      password_confirmation_mismatch: t('storage.error.passwordConfirmationMismatch'),
      invalid_current_password: t('storage.error.invalidCurrentPassword'),
      invalid_storage_password: t('storage.error.invalidStoragePassword'),
      password_rate_limited: t('storage.error.passwordRateLimited'),
      storage_state_conflict: t('storage.error.storageStateConflict'),
      key_save_uncertain: t('storage.error.keySaveUncertain'),
      storage_operation_failed: t('storage.error.storageOperationFailed'),
      storage_prepare_failed: t('storage.error.storagePrepareFailed'),
      legacy_source_changed: t('storage.error.legacySourceChanged'),
      legacy_data_requires_repair: t('storage.error.legacyDataRequiresRepair'),
      environment_unlock_failed: t('storage.error.environmentUnlockFailed'),
      system_unlock_failed: t('storage.error.systemUnlockFailed'),
      secure_transport_required: t('storage.error.secureTransportRequired'),
      invalid_request: t('storage.error.invalidRequest'),
      json_required: t('storage.error.jsonRequired')
    };
    error.hidden = false;
    error.textContent = messages[code] || messages.storage_operation_failed;
  }
  function render() {
    if (!status) return;
    var states = {
      UNINITIALIZED: t('storage.state.uninitialized'), LOCKED: t('storage.state.locked'),
      MIGRATING: t('storage.state.migrating'), RECOVERING: t('storage.state.recovering'),
      READY: t('storage.state.ready'), CLEANUP_PENDING: t('storage.state.cleanupPending'),
      FAILED: t('storage.state.failed'), CLOSED: t('storage.state.closed')
    };
    document.getElementById('storage-state').textContent = states[status.state] || states.FAILED;
    document.getElementById('storage-migration').hidden = !status.legacy_found || ready();
    document.getElementById('storage-cleanup').hidden = status.state !== 'CLEANUP_PENDING';
    document.getElementById('storage-workspace').hidden = !ready();
    var progress = status.progress || {};
    document.getElementById('storage-resuming').hidden = !progress.resuming || ready();
    var stages = {
      inventory: t('storage.stage.inventory'), archive: t('storage.stage.archive'),
      convert: t('storage.stage.convert'), publish: t('storage.stage.publish'), cleanup: t('storage.stage.cleanup')
    };
    var message = stages[progress.stage] || '';
    if (progress.total) message += ' · ' + (progress.completed || 0) + ' / ' + progress.total;
    if (progress.bytes_total) message += ' · ' + (progress.bytes_completed || 0).toLocaleString() + ' / ' + progress.bytes_total.toLocaleString() + ' B';
    document.getElementById('storage-progress').textContent = message;
    var setup = status.state === 'UNINITIALIZED';
    var unlock = (status.state === 'LOCKED' || status.state === 'FAILED') && status.password_configured;
    var change = ready() && status.password_configured;
    form.hidden = !setup && !unlock && !change;
    document.getElementById('storage-current-row').hidden = setup;
    document.getElementById('storage-new-row').hidden = unlock;
    document.getElementById('storage-confirm-row').hidden = unlock;
    current.required = !setup;
    next.required = confirm.required = !unlock;
    document.getElementById('storage-current-label').textContent = unlock ? t('storage.password') : t('storage.current');
    submit.textContent = setup ? t('storage.initialize') : unlock ? t('storage.unlock') : t('storage.change');
    submit.disabled = busy;
    if (lastErrorCode) showError(lastErrorCode);
    else if (status.error_code) showError(status.error_code);
  }
  function refresh() {
    return fetch('api/storage/status', { credentials: 'same-origin', cache: 'no-store' }).then(function (response) {
      if (!response.ok) throw new Error('storage_operation_failed');
      return response.json();
    }).then(function (value) {
      status = value;
      render();
      if (initializedHere && ready() && status.state !== 'CLEANUP_PENDING') window.location.replace('./');
    }).catch(function () { showError('storage_operation_failed'); });
  }
  form.addEventListener('submit', function (event) {
    event.preventDefault();
    if (busy || !status) return;
    error.hidden = true;
    lastErrorCode = '';
    var setup = status.state === 'UNINITIALIZED';
    var unlock = status.state === 'LOCKED' || status.state === 'FAILED';
    if (!unlock && next.value !== confirm.value) { showError('password_confirmation_mismatch'); return; }
    var operation = setup ? 'initialize' : unlock ? 'unlock' : 'password';
    var body = unlock ? { password: current.value } : { new_password: next.value, confirm_password: confirm.value };
    if (!setup && !unlock) body.current_password = current.value;
    busy = true;
    render();
    fetch('api/storage/' + operation, { method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) }).then(function (response) {
      return response.json().then(function (value) {
        if (!response.ok) { showError(value.error || 'storage_operation_failed'); return; }
        current.value = next.value = confirm.value = '';
        document.getElementById('storage-result').textContent = operation === 'password' ? t('storage.changed') : t('storage.preparing');
        initializedHere = operation !== 'password';
        status = value;
      });
    }).catch(function () { showError('storage_operation_failed'); }).finally(function () {
      busy = false;
      body = null;
      render();
      refresh();
    });
  });
  window.addEventListener('pagehide', function () { current.value = next.value = confirm.value = ''; });
  refresh();
  onLangChange(render);
  setInterval(refresh, 1500);
}());
