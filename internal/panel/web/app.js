// Admin panel client. Every action is a call to the admin API; the page holds
// no business rules of its own.
'use strict';

const status = document.getElementById('status');

function show(kind, text) {
	status.textContent = text;
	status.className = 'status ' + kind;
	clearTimeout(show.timer);
	show.timer = setTimeout(() => {
		status.textContent = '';
		status.className = 'status';
	}, 6000);
}

async function call(path, options = {}) {
	const response = await fetch('/api/v1/admin/' + path, {
		credentials: 'same-origin',
		...options,
		headers: { 'Content-Type': 'application/json', ...(options.headers || {}) },
	});
	if (response.status === 204) {
		return null;
	}
	const body = await response.json().catch(() => ({}));
	if (!response.ok) {
		const message = (body.error && body.error.message) || ('HTTP ' + response.status);
		throw new Error((body.error && body.error.code ? body.error.code + ': ' : '') + message);
	}
	return body;
}

const cell = (text) => {
	const el = document.createElement('td');
	el.textContent = text === null || text === undefined || text === '' ? '—' : String(text);
	return el;
};

const fmt = (value) => (value ? new Date(value).toLocaleString() : 'бессрочно');

function button(label, onClick, extraClass) {
	const el = document.createElement('button');
	el.type = 'button';
	el.textContent = label;
	el.className = extraClass || '';
	el.addEventListener('click', onClick);
	return el;
}

async function action(fn, okMessage) {
	try {
		await fn();
		show('ok', okMessage);
		return true;
	} catch (err) {
		show('error', err.message);
		return false;
	}
}

let users = [];
let channels = [];

async function loadChannels() {
	channels = (await call('channels')).channels;
	const body = document.getElementById('channels-body');
	body.textContent = '';
	for (const channel of channels) {
		const row = document.createElement('tr');
		row.append(cell(channel.id), cell(channel.number), cell(channel.name), cell(channel.stream_path));

		const state = cell(channel.is_active ? 'включён' : 'выключен');
		state.className = channel.is_active ? 'ok' : 'off';
		row.append(state);

		const actions = document.createElement('td');
		actions.append(
			button(channel.is_active ? 'Выключить' : 'Включить', async () => {
				if (await action(() => call('channels/' + channel.id, {
					method: 'PATCH',
					body: JSON.stringify({ is_active: !channel.is_active }),
				}), `Канал ${channel.name}: ${channel.is_active ? 'выключен' : 'включён'}`)) {
					loadChannels();
				}
			}),
		);
		row.append(actions);
		body.append(row);
	}
	fillSelect('grant-channel', channels.map((c) => [c.id, `${c.number} · ${c.name}`]));
}

async function loadUsers() {
	users = (await call('users')).users;
	const body = document.getElementById('users-body');
	body.textContent = '';
	for (const user of users) {
		const row = document.createElement('tr');
		row.append(cell(user.id), cell(user.login));

		const state = cell(user.is_active ? 'активен' : 'отключён');
		state.className = user.is_active ? 'ok' : 'off';
		row.append(state, cell(user.is_admin ? 'админ' : 'абонент'));

		const actions = document.createElement('td');
		actions.append(
			button(user.is_active ? 'Отключить' : 'Включить', async () => {
				if (await action(() => call('users/' + user.id, {
					method: 'PATCH',
					body: JSON.stringify({ is_active: !user.is_active }),
				}), `Пользователь ${user.login}: ${user.is_active ? 'отключён' : 'включён'}`)) {
					loadUsers();
				}
			}),
			button('Сменить пароль', async () => {
				const password = window.prompt(`Новый пароль для ${user.login} (минимум 8 символов)`);
				if (!password) {
					return;
				}
				await action(() => call('users/' + user.id, {
					method: 'PATCH',
					body: JSON.stringify({ password }),
				}), `Пароль ${user.login} обновлён`);
			}),
		);
		row.append(actions);
		body.append(row);
	}
	fillSelect('subs-user', users.map((u) => [u.id, u.login]));
	fillSelect('grant-user', users.map((u) => [u.id, u.login]));
}

function fillSelect(id, options) {
	const select = document.getElementById(id);
	const previous = select.value;
	select.textContent = '';
	for (const [value, label] of options) {
		const option = document.createElement('option');
		option.value = String(value);
		option.textContent = label;
		select.append(option);
	}
	if ([...select.options].some((o) => o.value === previous)) {
		select.value = previous;
	}
}

async function loadSubscriptions(userID) {
	if (!userID) {
		return;
	}
	document.getElementById('grant-user').value = String(userID);
	const data = await call(`users/${userID}/subscriptions`);
	const known = new Map(channels.map((c) => [c.id, c]));
	const body = document.getElementById('subs-body');
	body.textContent = '';
	if (!data.subscriptions.length) {
		const row = document.createElement('tr');
		row.append(cell('подписок нет'));
		body.append(row);
	}
	for (const sub of data.subscriptions) {
		const channel = known.get(sub.channel_id);
		const row = document.createElement('tr');
		const label = channel ? `${channel.number} · ${channel.name}` : `#${sub.channel_id}`;
		row.append(cell(label), cell(fmt(sub.expires_at)));

		const expired = sub.expires_at !== null && new Date(sub.expires_at) <= new Date();
		const state = cell(sub.channel_active === false ? 'канал выключен' : (expired ? 'истекла' : 'действует'));
		state.className = expired ? 'off' : 'ok';
		row.append(state);

		const actions = document.createElement('td');
		actions.append(
			button('Продлить на месяц', async () => {
				const expires = new Date(Date.now() + 30 * 24 * 3600 * 1000);
				await setSubscription(userID, sub.channel_id, expires.toISOString());
			}),
			button('Отозвать', async () => {
				if (await action(() => call(`users/${userID}/subscriptions/${sub.channel_id}`, { method: 'DELETE' }),
					'Подписка отозвана')) {
					loadSubscriptions(userID);
				}
			}, 'danger'),
		);
		row.append(actions);
		body.append(row);
	}
}

async function setSubscription(userID, channelID, expiresAt) {
	if (await action(() => call(`users/${userID}/subscriptions/${channelID}`, {
		method: 'PUT',
		body: JSON.stringify({ expires_at: expiresAt }),
	}), 'Подписка обновлена')) {
		loadSubscriptions(userID);
	}
}

document.getElementById('channel-form').addEventListener('submit', async (event) => {
	event.preventDefault();
	const data = new FormData(event.target);
	const ok = await action(() => call('channels', {
		method: 'POST',
		body: JSON.stringify({
			number: Number(data.get('number')),
			name: data.get('name'),
			stream_path: data.get('stream_path'),
		}),
	}), 'Канал добавлен');
	if (ok) {
		event.target.reset();
		loadChannels();
	}
});

document.getElementById('user-form').addEventListener('submit', async (event) => {
	event.preventDefault();
	const data = new FormData(event.target);
	const ok = await action(() => call('users', {
		method: 'POST',
		body: JSON.stringify({
			login: data.get('login'),
			password: data.get('password'),
			is_admin: data.get('is_admin') === 'on',
		}),
	}), 'Пользователь создан');
	if (ok) {
		event.target.reset();
		loadUsers();
	}
});

document.getElementById('subs-form').addEventListener('submit', (event) => {
	event.preventDefault();
	loadSubscriptions(new FormData(event.target).get('user_id')).catch((err) => show('error', err.message));
});

document.getElementById('grant-form').addEventListener('submit', (event) => {
	event.preventDefault();
	const data = new FormData(event.target);
	const expires = data.get('expires_at');
	setSubscription(data.get('user_id'), data.get('channel_id'), expires ? new Date(expires).toISOString() : null);
});

document.getElementById('clear-expiry').addEventListener('click', () => {
	document.querySelector('#grant-form input[name="expires_at"]').value = '';
});

document.getElementById('refresh-cache').addEventListener('click', async () => {
	try {
		const stats = await call('cache');
		document.getElementById('cache-stats').textContent = JSON.stringify(stats, null, 2);
	} catch (err) {
		show('error', err.message);
	}
});

loadChannels()
	.then(loadUsers)
	.then(() => loadSubscriptions(document.getElementById('subs-user').value))
	.catch((err) => show('error', err.message));
