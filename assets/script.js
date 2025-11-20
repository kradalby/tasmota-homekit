(function () {
  function formatTime(value) {
    if (!value) {
      return "unknown";
    }
    const date = new Date(value);
    if (isNaN(date)) {
      return value;
    }
    return date.toLocaleTimeString();
  }

  function updatePlugCard(data) {
    const card = document.querySelector('[data-plug-id="' + data.plug_id + '"]');
    if (!card) {
      return;
    }

    card.classList.toggle('on', data.on);
    card.classList.toggle('off', !data.on);

    const status = card.querySelector('[data-role="status-text"]');
    if (status) {
      status.textContent = 'Status: ' + (data.on ? 'ON' : 'OFF') + ' | Last updated: ' + formatTime(data.last_updated);
    }

    const indicator = card.querySelector('[data-role="connection-indicator"]');
    if (indicator) {
      indicator.classList.remove('connected', 'stale', 'disconnected');
      indicator.classList.add(data.connection_state || 'disconnected');
    }

    const connectionText = card.querySelector('[data-role="connection-text"]');
    if (connectionText) {
      connectionText.textContent = data.connection_note || '';
    }

    // Update electrical stats if present
    const powerEl = card.querySelector('[data-role="power-value"]');
    if (powerEl && data.power !== undefined) {
      powerEl.textContent = data.power.toFixed(1) + ' W';
    }

    const voltageEl = card.querySelector('[data-role="voltage-value"]');
    if (voltageEl && data.voltage !== undefined) {
      voltageEl.textContent = data.voltage.toFixed(1) + ' V';
    }

    const currentEl = card.querySelector('[data-role="current-value"]');
    if (currentEl && data.current !== undefined) {
      currentEl.textContent = data.current.toFixed(2) + ' A';
    }

    const energyEl = card.querySelector('[data-role="energy-value"]');
    if (energyEl && data.energy !== undefined) {
      energyEl.textContent = data.energy.toFixed(3) + ' kWh';
    }

    const actionInput = card.querySelector('[data-role="action-input"]');
    const button = card.querySelector('[data-role="toggle-button"]');
    if (actionInput && button) {
      if (data.on) {
        actionInput.value = 'off';
        button.textContent = 'Turn Off';
        button.classList.remove('on');
        button.classList.add('off'); // Red for Turn Off
      } else {
        actionInput.value = 'on';
        button.textContent = 'Turn On';
        button.classList.remove('off');
        button.classList.add('on'); // Green for Turn On
      }
    }
  }

  document.addEventListener('DOMContentLoaded', function () {
    const source = new EventSource('/events');
    source.onmessage = function (event) {
      try {
        const data = JSON.parse(event.data);
        updatePlugCard(data);
      } catch (err) {
        console.error('invalid SSE payload', err);
      }
    };
  });
})();
