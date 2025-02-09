
const FACE = 'face';
const COVER = 'cover';

const SECOND = 1000;
const BUTTON_LEFT = 0;

let players = {};
let tableWidth = 0;
let tableHeight = 0;

let isKeyTPressed = false;
let isKeyOPressed = false;

let socket = null;

function getSession() {
    const cookies = document.cookie.split('; ');
    const sessionCookie = cookies.find(cookie => cookie.startsWith('session='));
    if (sessionCookie) {
        const encodedValue = sessionCookie.split('=')[1];
        return JSON.parse(atob(encodedValue));
    }
    return {'user_id': '', 'name': ''};
}

class AJAX {
    constructor() {
        this.successHandler = null;
        this.errorHandler = null;
    }

    success(callback) {
        this.successHandler = callback;
        return this;
    }

    error(callback) {
        this.errorHandler = callback;
        return this;
    }

    async request(method, url, body = null) {
        try {
            const response = await fetch(url, {
                method: method,
                headers: { 'Content-Type': 'application/json' },
                body: body ? JSON.stringify(body) : null,
            });
            if (!response.ok) {
                const err = new Error('HTTP error');
                err.status = response.status;
                err.body = response.text();
                throw err;
            }
            const data = await response.json();
            if (this.successHandler) {
                this.successHandler(data);
            }
        } catch (error) {
            if (this.errorHandler) {
                this.errorHandler(error);
                return;
            }
            console.error('AJAX.fetch error: ', error);
        }
    }

    get(url) { this.request('GET', url); }
    postJSON(url, obj) { this.request('POST', url, obj); }
}

function ajax() { return new AJAX() };

// Rect represents a rect over an HTML element
class Rect {
    constructor(item) {
        this.item = item;
    }

    left() { return parseInt(window.getComputedStyle(this.item).left); }

    top() { return parseInt(window.getComputedStyle(this.item).top); }

    width() { return parseInt(window.getComputedStyle(this.item).width); }

    height() {return parseInt(window.getComputedStyle(this.item).height); }

    centerX() {
        return this.left() + this.width()/2;
    }
    centerY() {
        return this.top() + this.height()/2;
    }

    centerWithin(rect) {
        return this.centerX() >= rect.left() && this.centerX() <= rect.left() + rect.width() &&
                this.centerY() >= rect.top() && this.centerY() <= rect.top() + rect.height();
    }
}

function handleItemDrop(item) {
    const slots = document.querySelectorAll('.player_slot');
    switch (item.info.class) {
    case 'chip':
        // XXX: accountChip has to be called in exactly in this handler.
        // Otherwise the following situation will not be handled correctly:
        // - when a chip that is being dragged stops under another chip.
        // In this case the event will be called with the top most item with
        // regard to z-index.
        accountChip(item, slots);
        slots.forEach(updateSlotsWithMoney);
        break;
    case 'card':
        const rect = new Rect(item);
        // console.log(`DEBUG chip id=${chip.id} accountChip`);
        for (let slot of slots) {
            if (slot.playerElem == null || slot.playerElem === undefined) {
                continue;
            }
            const owner_id = slot.playerElem.info.owner_id;
            if (getSession().user_id != owner_id) {
                continue
            }
            const slotRect = new Rect(slot);
            if (rect.centerWithin(slotRect)) {
                takeCard(item);
                return;
            }
        }

        const showSlot = document.getElementById('open-slot');
        if (rect.centerWithin(new Rect(showSlot))) {
            showCard(item);
        }
        break;
    }
}

function onItemMouseDown(e, item) {
    if (e.button != BUTTON_LEFT) {
        return;
    }
    let initialMouseX = e.clientX;
    let initialMouseY = e.clientY;

    let initialItemX = parseInt(item.style.left);
    let initialItemY = parseInt(item.style.top);

    function onMouseMove(event) {
        const deltaX = event.clientX - initialMouseX;
        const deltaY = event.clientY - initialMouseY;

        const left = parseInt(initialItemX + deltaX);
        const top = parseInt(initialItemY + deltaY);

        // console.info(item.id, left, top, tableWidth, tableHeight);
        const itemRect = new Rect(item);
        console.log(left, top, tableWidth + itemRect.width() / 2);
        if ((left < 0 || left > tableWidth - itemRect.width() / 2) ||
            (top < 0 || top > tableHeight - itemRect.height() / 2)
        ) {
            return; // disallow to move items outside the table
        }

        item.style.left = `${left}px`;
        item.style.top = `${top}px`;

        item.info.x = left;
        item.info.y = top;

        ajax().postJSON(`${window.location.pathname}/update`, item.info);
    }
    document.addEventListener('pointermove', onMouseMove);

    document.addEventListener('pointerup', () => {
        ajax().postJSON(`${window.location.pathname}/update`, item.info);
        handleItemDrop(item);
        // cleanup for this drag-n-drop
        document.removeEventListener('pointermove', onMouseMove);
    }, { once: true });
}

function newItem(cls, info, x, y) {
    const item = document.createElement('div');
    item.classList.add(cls);

    item.id = `item-${info.id}`
    item.info = info;
    item.style.top = `${y}px`;
    item.style.left = `${x}px`;

    item.ondragstart = () => false;
    // Make the item draggable
    item.addEventListener('pointerdown', (e) => { onItemMouseDown(e, item); });

    item.render = () => {};

    return item;
}

function renderCard(card) {
    let text = '';
    let color = 'black';
    let side = card.info.side;
    let css = `card-${side}`

    card.classList.remove('card-cover', 'card-face', 'owned');
    card.style.borderColor = '';
    if (card.info.owner_id != '') {
        card.classList.add('owned');
        card.style.borderColor = players[card.info.owner_id].color || 'purple';
        // console.info(players[card.info.owner_id]);
    }
    if (side == FACE) {
        text = `${card.info.rank} ${card.info.suit}`;
        color = card.info.suit == '♥' || card.info.suit == '♦' ? 'red': 'black';
    }
    card.innerText = text;
    card.classList.add(css);
    card.style.color = color;
}

function onCardDblClick(e, card) {
    console.log('DEBUG onCardDblClick', e.button);
    if (e.button != BUTTON_LEFT) {
        return;
    }
    if (card.info.owner_id != '' && card.info.owner_id != getSession().user_id) {
        return; // can't turn other player cards cards
    }
    card.info.side = card.info.side == COVER ? FACE: COVER;
    ajax().success((resp) => {
        updateItem(resp.updated);
    }).postJSON(`${window.location.pathname}/update`, card.info)
}

let lastTapTime = 0;
const TAP_MAX_DURATION_MS = 300;

function newCard(info, x, y) {
    const card = newItem('card', info, x, y);
    card.addEventListener('click', (e) => {
        if (e.button != BUTTON_LEFT) {
            return;
        }
        if (e.ctrlKey || isKeyTPressed || e.metaKey) {
            takeCard(card);
        }
        if (e.shiftKey || isKeyOPressed) {
            showCard(card);
        }
    });
    card.addEventListener('dblclick', (e) => { onCardDblClick(e, card) });

    card.addEventListener('touchend', (e) => {
        const currentTime = new Date().getTime();
        const tapInterval = currentTime - lastTapTime;
        ajax().error(()=>{}).get(`/log?action=touchend&ct=${currentTime}&&tapInterval=${tapInterval}`)
        if (tapInterval < TAP_MAX_DURATION_MS) {
            ajax().
                error(()=>{}).
                get(`/log?action=doubletap&ct=${currentTime}&&tapInterval=${tapInterval}&button=${e.button}`);
            e.button = BUTTON_LEFT;
            onCardDblClick(e, card);
        }
        lastTapTime = currentTime;
    });


    card.render = () => {  renderCard(card); };
    card.render();
    return card;
}

function accountChip(chip, slots) {
    if (chip == null) {
        return;
    }
    const rect = new Rect(chip);
    // console.log(`DEBUG chip id=${chip.id} accountChip`);
    for (let slot of slots) {
        if (slot.chips == null) {
            continue;
        }
        slotRect = new Rect(slot);
        if (chip.id in slot.chips) {
            delete slot.chips[chip.id];
        }
        if (rect.centerWithin(slotRect)) {
            slot.chips[chip.id] = chip;
            // console.log(`${chip.info.class} id=${chip.id} within player ${slot.dataset.index} slot`);
        } else {
            // console.log(`${chip.info.class} id=${chip.id} outside of any player slot`);
        }
    }
}

function newChip(info, x, y) {
    const chip = newItem('chip', info, x, y);
    chip.classList.add(`chip-${info.color}`);
    chip.innerText = info.val;

    return chip;
}

function newDealer(info, x, y) {
    const item = newItem('dealer', info, x, y);
    item.innerText = 'Dealer';
    return item;
}

function newPlayer(info, x, y) {
    const item = newItem('player', info, x, y);
    // HACK: gets data from global state due to .color property conflict
    const player = players[info.owner_id];
    item.classList.add(player.skin);
    item.innerText = player.Name;

    const slot = document.getElementById(`slot-${player.index}`);
    slot.playerElem = item;
    slot.chips = {};

    item.render = () => {
        // HACK
        item.style.left = '';   // use from css
        item.style.top = '';    // use from css
    };
    item.render();
    return item;
}

function updateSlotsWithMoney(slot) {
    if (slot.chips == null || slot.playerElem == null) {
        return;
    }
    const vals = Object.values(slot.chips).map(chip => chip.info.val);
    const total = vals.reduce((s, x) => s + x, 0);;
    const player = players[slot.playerElem.info.owner_id];
    slot.playerElem.innerText = `${player.Name}: ${total}$`;
}

function updateItem(src) {
    if (src === null || src === undefined) {
        return;
    }
    if (src.id === null || src.id === undefined) {
        console.log('warn bad id', src);
        return;
    }
    let item = document.getElementById(`item-${src.id}`);
    if (item == null) {
        item = createItem(src);
    }
    item.info = src;
    item.style.top = `${src.y}px`;
    item.style.left = `${src.x}px`;
    item.render();
}

function updateItems(items) {
    const slots = document.querySelectorAll('.player_slot');
    for (let it of items) {
        updateItem(it);
        if (it.class == 'chip') {
            const item = document.getElementById(`item-${it.id}`);
            accountChip(item, slots);
        }
    }
    slots.forEach(updateSlotsWithMoney);
}

function updateTable(resp) {
    players = resp.players;
    updateItems(resp.items);
}

function refresh(items) {
    ajax().success((resp) => {
        updateTable(resp)
    }).error((status, msg) => {
        if (status === 401) {
            window.location = '/';
        }
        console.error('refersh', status, msg);
    }).get(`${window.location.pathname}/state`);
}

function createItem(info) {
    const table = document.getElementById('card-table');
    let item = null;
    switch (info.class) {
    case 'card':
        item = newCard(info, info.x, info.y);
        break;
    case 'chip':
        item = newChip(info, info.x, info.y);
        break;
    case 'dealer':
        item = newDealer(info, info.x, info.y);
        break;
    case 'player':
        item = newPlayer(info, info.x, info.y);
        break;
    default:
        throw new Exception(`unknown item class: ${info.class}`)
    }
    table.appendChild(item);
    return item;
}

function takeCard(card) {
    if (card.info.owner_id != '') {
        return; // already owned
    }
    ajax().success((resp) => {
        updateItem(resp.updated);
    }).postJSON(`${window.location.pathname}/take_card`, {id: card.info.id});
}

function showCard(card) {
    if (card.info.owner_id != getSession().user_id) {
        return; // can't show not owned cards
    }
    ajax().success((resp) => {
        updateItem(resp.updated);
    }).postJSON(`${window.location.pathname}/show_card`, {id: card.info.id});
}

function isKeyPressed(e, key) {
    try {
        return e.key.toLowerCase() === key;
    } catch {
        return false;
    }
}

function listenPushes() {
    const sock = new WebSocket(`ws://${window.location.host}${window.location.pathname}/listen`);
    sock.onopen = () => {
        console.log('websocket connected');
        hideElem(document.getElementById('error-banner'));
    };
    sock.onclose = () => {
        console.log('websocket disconnected');
        showError('OFFLINE. Try to refresh');
        setTimeout(() => { socket = listenPushes(); }, 10 * SECOND);
    };
    sock.onerror = (err) => {
        console.error('websocket error:', err);
    };
    sock.onmessage = (event) => {
        let resp = null;
        try {
            resp = JSON.parse(event.data)
        } catch (e) {
            // non-JSON payload?
            console.log("push error:",e, event.data);
            return;
        }
        switch (resp.type) {
        case 'player_joined':
            updateTable(resp);

            break;
        case 'update_items':
            updateItems(resp.items);
            break;
        case 'refresh':
            location.reload();
            break;
        default:
            console.log("push unknown:", resp);
        }
    };
    return sock;
}

function showElem(elem) {
    elem.style.display = 'block';
}

function hideElem(elem) {
    elem.style.display = 'none';
}

function showError(text) {
    const banner = document.getElementById('error-banner');
    banner.innerHTML = `<p>${text}</p>`;
    showElem(banner);
    return banner;
}

function blockTable(table) {
    showError('Portrait mode is not supported. Switch to landscape!');
    for (let elem of table.children) {
        hideElem(elem);
    }
}

function isPortraitMode() { return window.innerWidth < window.innerHeight; }


// function handleDoubleTap(event) {
//     // Your double-tap handling logic here
//     console.log('Element was double-tapped.');
// }

function start() {
    const theTable = document.getElementById('card-table');
    window.addEventListener("resize", function() {
        if (isPortraitMode()) {
            blockTable(theTable);
        } else {
            location.reload();
        }
    });
    if (isPortraitMode()) {
        blockTable(theTable);
        return;
    } else {
        hideElem(document.getElementById('error-banner'));
    }

    const tableRect = new Rect(theTable);
    tableWidth = tableRect.width();
    tableHeight = tableRect.height();
    document.addEventListener('keydown', (event) => {
        if (isKeyPressed(event, 't')) {
            isKeyTPressed = true;
        }
        if (isKeyPressed(event, 'o')) {
            isKeyOPressed = true;
        }
    });
    document.addEventListener('keyup', (event) => {
        if (isKeyPressed(event, 't')) {
            isKeyTPressed = false;
        }
        if (isKeyPressed(event, 'o')) {
            isKeyOPressed = true;
        }
    });

    ajax().success((resp) => {
        console.info('initial table fetch:', resp);
        updateTable(resp);
        socket = listenPushes();
    }).get(`${window.location.pathname}/state?cw=${window.screen.availWidth}&ch=${window.screen.availHeight}&iw=${window.innerWidth}&ih=${window.innerHeight}`);
}
