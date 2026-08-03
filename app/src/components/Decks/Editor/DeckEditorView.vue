<script setup lang="ts">
import { computed, nextTick, onMounted, onUnmounted, reactive, watch } from 'vue';
import { useRoute, useRouter } from 'vue-router';
import { unwrapErrorMessage, useClient } from '@/api';
import type { CardDeckMeta, CardDeckVersion, ResourceVisibility } from '@/api_models';
import type { CardCanvasTheme, CardContentElementTheme, CardNode, ContentSummary } from '@/content';
import { addModelNode } from '@/content';
import { useStorage, type DeckEditorHistoryMetaEntry } from '@/storage/storage';
import GenericButton from '@/components/App/Inputs/GenericButton.vue';
import LoadingMessage from '@/components/App/Messages/LoadingMessage.vue';
import EditorContentExporter from './EditorModals/EditorContentExporter.vue';
import EditorContentImporter from './EditorModals/EditorContentImporter.vue';
import DeckCardList from './EditorCardNavigationList.vue';
import EditorScreenOverlay from './EditorScreenOverlay.vue';
import EditorVersionControlModal from './EditorModals/EditorVersionControlModal.vue';
import OverlayErrorMessage from '@/components/App/Messages/OverlayErrorMessage.vue';
import CardFaceEditor from './FaceEditor/CardFaceEditor.vue';
import EditorVersionPublishModal from './EditorModals/EditorVersionPublishModal.vue';
import DeckEditorHeader from './Toolbar/DeckEditorHeader.vue';
import DeckEditorSummary from './Toolbar/DeckEditorSummary.vue';
import DeckEditorAutosaveIndicator from './Toolbar/DeckEditorAutosaveIndicator.vue';
import DeckEditorMenu from './Toolbar/DeckEditorMenu.vue';
import DeckEditorMenuEntry from './Toolbar/DeckEditorMenuEntry.vue';
import EditorDeckDetailsModal from './EditorModals/EditorDeckDetailsModal.vue';
import { reactiveSnapshot } from '@/proxies';
import DeckEditorElementColorDropdown from './Toolbar/DeckEditorElementColorDropdown.vue';
import DeckEditorElementButton from './Toolbar/DeckEditorElementButton.vue';
import { Shortcuts } from '@/shortcuts';
import { isInteractive } from '@/dom';
import EditorShortcutsModal from './EditorModals/EditorShortcutsModal.vue';
import { appSetTitle } from '@/app';

const route = useRoute();
const router = useRouter();
const store = useStorage();
const client = useClient();

const defaultDeckMeta = () => ({
	summary: {
		name: 'Unnamed deck',
		description: null as string | null,
	},
	visibility: 'HIDDEN' as ResourceVisibility,
});

const maxEditHistorySize = 20;
const autosaveIntervalMs = 1_000;

enum EditedSide {
	Front = 1,
	Back = 2,
};

const state = reactive({

	content: {
		meta: defaultDeckMeta(),
		cards: [] as CardNode[],
	},

	origin: {
		deckID: null as string | null,
		collectionID: null as string | null,
		created: null as string | null,
		updated: null as string | null,
	},

	editor: {
		ready: false,
		error: null as string | null,
		view: {
			cardIdx: 0,
			side: null as EditedSide | null,
		},
		modals: {
			versions: false,
			importer: false,
			exporter: false,
			publish: false,
			details: false,
			shortcuts: false,
		},
		history: {
			entries: [] as ResumableState[],
			point: null as DeckEditorHistoryMetaEntry | null,
		},
		changes: {
			summary: false,
			cards: false,
			meta: false,
		},
		snapshots: {
			timer: null as NodeJS.Timeout | null,
			lock: false,
			writtenVersion: null as DeckEditorHistoryMetaEntry | null,
			loadedVersion: null as DeckEditorHistoryMetaEntry | null,
		},
		shortcuts: null as Shortcuts | null,
	},
});

const editorReady = computed(() => state.editor.ready && !state.editor.error);
const contentEdited = computed(() => state.editor.changes.cards || state.editor.changes.summary || state.editor.changes.meta);
const changesSaved = computed(() => !state.editor.snapshots.timer && !!(state.editor.snapshots.loadedVersion || state.editor.snapshots.writtenVersion));
const deckPublished = computed(() => !!state.origin.deckID);
const localDeckID = computed(() => state.origin.deckID || 'new');
const modalsOpen = computed(() => Array.from(Object.values(state.editor.modals)).filter(val => val).length);

const historyEditIdx = computed((): number => {

	if (!state.editor.history.point) {
		return 0;
	}

	const idx = state.editor.history.entries.findIndex(item => item.timestamp === state.editor.history.point!.timestamp);
	return idx > 0 ? idx : 0;
})

const historyCanUndo = computed((): boolean =>
	state.editor.history.entries.length > 0 &&
	historyEditIdx.value < state.editor.history.entries.length - 1);

const historyCanRedo = computed((): boolean =>
	state.editor.history.entries.length > 0 &&
	!!state.editor.history.point &&
	historyEditIdx.value > 0);

const editedCardNode = computed(() => {

	const entry = state.content.cards[state.editor.view.cardIdx];
	if (!entry) {
		return { id: null, front: null, back: null };
	}

	return { id: entry.id, front: entry.front, back: entry.back };
});

const editedCardFace = computed(() => {
	switch (state.editor.view.side) {
		case EditedSide.Front:
			return editedCardNode.value.front || null;
		case EditedSide.Back:
			return editedCardNode.value.back || null;
		default:
			return null;
	}
});

const insertableContentNodes = computed(() => {
	const typeSet = new Set(editedCardFace.value?.content?.map(item => item.type));
	return {
		title: !!editedCardFace.value && !typeSet.has('title'),
		image: !!editedCardFace.value && !typeSet.has('image'),
		textbox: !!editedCardFace.value && !typeSet.has('textbox'),
		poll: !!editedCardFace.value && state.editor.view.side === EditedSide.Front && !typeSet.has('poll'),
	};
});

const cardSelectorList = computed(() => state.content.cards.map(item => item.front));

const addCard = (node: CardNode) => {
	state.content.cards.push(node);
	state.editor.view.cardIdx = state.content.cards.length - 1;
	state.editor.view.side = null;
};

const createCard = () => {
	const newFace = () => ({ content: [] });
	addCard({ id: crypto.randomUUID(), front: newFace(), back: newFace() });
};

const selectCard = (idx: number) => {
	state.editor.view.cardIdx = idx;
	state.editor.view.side = null;
};

const duplicateCard = (idx: number) => {
	const clonedNode = state.content.cards[idx];
	if (!clonedNode) {
		return;
	}
	addCard(reactiveSnapshot(clonedNode));
};

const reorderCard = (idx: number, delta: number) => {

	const newIdx = idx + delta;
	if (newIdx < 0 || newIdx >= (state.content.cards?.length ?? 0)) {
		return;
	}

	const node = state.content.cards[idx];

	state.content.cards[idx] = state.content.cards[newIdx];
	state.content.cards[newIdx] = node;

	selectCard(newIdx);
};

const removeCard = (idx: number) => {

	if (!confirm('Remove card?')) {
		return;
	}

	state.content.cards.splice(idx, 1);

	if (state.editor.view.cardIdx >= state.content.cards.length) {
		state.editor.view.cardIdx = state.content.cards.length - 1;
	}

	state.editor.view.side = null;
};

interface ResumableState extends DeckEditorHistoryMetaEntry {
	editor: {
		view: Pick<typeof state['editor']['view'], 'cardIdx'>;
	};
	origin: typeof state.origin,
	content: typeof state['content'];
};

const cloneEditorState = (): ResumableState => reactiveSnapshot({
	deck_id: localDeckID.value,
	timestamp: new Date(),
	content: state.content,
	editor: {
		view: {
			cardIdx: state.editor.view.cardIdx,
		},
	},
	origin: state.origin,
});;

const addHistoryVersion = (version: ResumableState) => {

	const limit = Math.min(state.editor.history.entries.length - historyEditIdx.value, maxEditHistorySize);

	state.editor.history.entries = state.editor.history.entries.slice(historyEditIdx.value, historyEditIdx.value + limit);
	state.editor.history.entries.unshift(version);

	if (state.editor.history.point) {
		const after = new Date(state.editor.history.point.timestamp.getTime() + 1);
		store.decks.editor.history.clear(localDeckID.value, after)
			.catch(error => console.debug('Trim persistent editor history:', error));
	}

	state.editor.history.point = null;
};

const editorHistoryBack = () => {

	const version = state.editor.history.entries[historyEditIdx.value + 1];
	if (!version) {
		console.warn('Cant execute editor undo');
		return;
	}

	applyEditorHistoryVersion(version);
};

const editorHistoryForward = () => {

	if (!state.editor.history.point) {
		editorHistorySkipToHead();
		return;
	}

	if (historyEditIdx.value <= 1) {
		editorHistorySkipToHead();
		return;
	}

	applyEditorHistoryVersion(state.editor.history.entries[historyEditIdx.value - 1]);
};

const editorHistorySkipToHead = () => {

	const head = state.editor.history.entries[0];
	if (head) {
		applyEditorHistoryVersion(head);
	}

	state.editor.history.point = null;
};

const applyEditorHistoryVersion = (version: ResumableState) => {

	const { ready } = state.editor;

	state.editor.ready = false;

	state.editor.history.point = { deck_id: version.deck_id, timestamp: version.timestamp };
	state.content = reactiveSnapshot(version.content);

	nextTick(() => state.editor.ready = ready);
};

const queueStateSnapshot = () => {

	if (!state.editor.ready || state.editor.snapshots.lock) {
		return;
	}

	if (state.editor.snapshots.timer) {
		clearTimeout(state.editor.snapshots.timer);
	}

	state.editor.snapshots.timer = setTimeout(async () => {
		const snapshot = cloneEditorState();
		addHistoryVersion(snapshot);
		await writeStateSnapshot(snapshot);
		state.editor.snapshots.timer = null;
	}, autosaveIntervalMs);
};

const writeStateSnapshot = async (snapshot: ResumableState) => {

	if (state.editor.snapshots.lock) {
		return;
	}

	state.editor.snapshots.lock = true;

	const { error } = await store.decks.editor.history.add(snapshot)
		.then(() => ({ error: null }))
		.catch(error => ({ error }));

	if (error) {
		console.error('editor.snapshots.push', error);
		state.editor.snapshots.lock = false;
		return;
	}

	state.editor.snapshots.writtenVersion = { deck_id: snapshot.deck_id, timestamp: snapshot.timestamp };
	state.editor.snapshots.lock = false;
};

const restoreStateSnapshot = async () => {

	const entries = await store.decks.editor.history.versions<ResumableState>(localDeckID.value, maxEditHistorySize).catch(() => null);
	if (!entries?.length) {
		state.editor.snapshots.loadedVersion = null;
		return;
	}

	const latest = structuredClone(entries[0]);

	state.origin.created = latest.origin.created;
	state.origin.updated = new Date().toISOString();
	state.origin.collectionID = latest.origin.collectionID || state.origin.collectionID;

	state.content = latest.content;
	state.editor.view.cardIdx = latest.editor.view.cardIdx;

	state.editor.history.entries = entries;
	state.editor.changes = { meta: true, summary: true, cards: true };
	state.editor.snapshots.loadedVersion = { deck_id: latest.deck_id, timestamp: latest.timestamp };
};

const clearStateSnapshot = async () => {

	const { ready } = state.editor;

	state.editor.ready = false;

	if (state.editor.snapshots.timer) {
		clearTimeout(state.editor.snapshots.timer);
	}

	await store.decks.editor.history.clear(localDeckID.value)
		.catch(error => console.error('clearStateSnapshot', error));

	state.editor.snapshots.writtenVersion = null;
	state.editor.snapshots.loadedVersion = null;

	nextTick(() => state.editor.ready = ready);
};

const fetchRemoteState = async (deckID: string) => {

	const { data, error } = await client.decks.load(deckID);
	if (!data || error) {
		state.editor.error = unwrapErrorMessage(error);
		return;
	}

	state.content = {
		meta: {
			summary: {
				name: data.name,
				description: data.description || null,
			},
			visibility: data.visibility,
		},
		cards: data.cards,
	};

	state.origin = {
		deckID: data.id,
		collectionID: data.collection_id,
		created: data.created,
		updated: data.updated,
	};

	state.editor.changes = { meta: false, summary: false, cards: false };
	state.editor.view.cardIdx = 0;
};

const watchContentEdits = () => {

	watch(() => state.content.meta, () => {

		if (!state.editor.ready) {
			return;
		}

		state.editor.changes.meta = true;
		queueStateSnapshot();

	}, { deep: true });

	watch(() => state.content.meta.summary, () => {

		if (!state.editor.ready) {
			return;
		}

		state.editor.changes.summary = true;
		queueStateSnapshot();

	}, { deep: true });

	watch(() => state.content.cards, () => {

		if (!state.editor.ready) {
			return;
		}

		state.editor.changes.cards = true;
		queueStateSnapshot();

	}, { deep: true });
};

const updateAppTitle = () => {

	const name = state.content.meta.summary.name || 'Unnamed';
	const title = deckPublished.value ? name : `Draft: ${name}`;

	appSetTitle(`${title} | Deck editor`);
};

const patchDeckSummary = (patch: ContentSummary) => {
	state.content.meta.summary = {
		name: patch.name || defaultDeckMeta().summary.name,
		description: patch.description || null,
	};
};

const handleCardCanvasThemeUpdate = (opts: Partial<CardCanvasTheme>) => {

	if (!editedCardFace.value) {
		return;
	}

	editedCardFace.value.theme = {
		... editedCardFace.value?.theme || {},
		card: {
			... editedCardFace.value?.theme?.card || {},
			... opts,
		},
	};
};

const handleCardContentElementThemeUpdate = (opts: Partial<CardContentElementTheme>) => {

	if (!editedCardFace.value) {
		return;
	}

	editedCardFace.value.theme = {
		... editedCardFace.value?.theme || {},
		interactives: {
			... editedCardFace.value?.theme?.interactives || {},
			... opts,
		},
	};
};

const applyPulledVersion = (version: CardDeckVersion) => {

	state.content.meta.summary = {
		name: version.content.summary.name || state.content.meta.summary.name,
		description: version.content.summary.description || state.content.meta.summary.description,
	};

	state.content.cards = version.content.cards;
	state.origin.updated = version.created;

	state.editor.changes = { meta: false, summary: false, cards: false };
	state.editor.view.cardIdx = 0;
};

const applyPublishedMeta = async (meta: CardDeckMeta) => {

	state.editor.ready = false;

	await clearStateSnapshot();

	if (!state.origin.deckID) {
		router.push(`/decks/editor/${meta.id}`);
	}

	state.origin = {
		deckID: meta.id,
		collectionID: meta.collection_id,
		created: meta.created,
		updated: meta.updated,
	};

	state.content.meta = {
		summary: {
			name: meta.name,
			description: meta.description || null,
		},
		visibility: meta.visibility,
	};

	state.editor.changes = { meta: false, summary: false, cards: false };

	nextTick(() => state.editor.ready = true);
};

const dropLocalChanges = async () => {

	if (!confirm('Drop all local changes?')) {
		return;
	}

	state.editor.ready = false;
	state.editor.error = null;

	state.editor.changes = { meta: false, summary: false, cards: false };
	state.editor.history = { entries: [], point: null };

	await clearStateSnapshot();

	if (state.origin.deckID) {
		await fetchRemoteState(state.origin.deckID);
	} else {
		state.content.meta = defaultDeckMeta();
		state.content.cards = [];
	}

	nextTick(() => state.editor.ready = !state.editor.error);
};

const deleteDeckAndExit = async () => {

	if (!state.origin.deckID) {
		return;
	}

	if (!confirm('Delete deck?')) {
		return;
	}

	const { error } = await client.decks.remove(state.origin.deckID);
	if (error) {
		console.error('Unable to delete collection deck:', error.message);
		return;
	}

	await clearAndExitEditor();
};

const duplicateDeck = async () => {

	const { collectionID } = state.origin;

	if (!collectionID) {
		console.error('Unable to duplicate a deck without a related collection ID');
		return;
	}

	state.origin.deckID = null;
	state.origin.created = new Date().toISOString();
	state.origin.updated = state.origin.created;
	state.content.meta.summary.name += ' - Copy';

	router.push(`/decks/editor?collection_id=${collectionID}`);

	queueStateSnapshot();

	//	ideally this should be handled by nextTick, but it doesn't fucking work,
	//	as per usual in the world of javascript
	setTimeout(updateAppTitle, 250);
};

const openPlayView = () => {
	if (!state.origin.deckID) {
		return;
	}
	window.open(`/play/deck/${state.origin.deckID}`, '_blank');
};

const backHref = computed(() => state.origin?.collectionID ? `/collection/${state.origin.collectionID}` : '/collections');

const clearAndExitEditorPrompt = () => {

	if (contentEdited.value && !confirm('Really discard your changes?')) {
		return;
	}

	clearAndExitEditor();
};

const clearAndExitEditor = async () => {
	await clearStateSnapshot();
	exitEditor();
};

const saveAndExitEditor = async () => {
	await writeStateSnapshot(cloneEditorState());
	exitEditor();
};

const exitEditor = () => router.push(backHref.value);

const registerShortcuts = () => {
	state.editor.shortcuts = new Shortcuts([
		{
			title: 'Publish deck changes',
			ctrl: true, key: 'u',
			action: () => editorReady.value && !modalsOpen.value ?
				state.editor.modals.publish = true : void 0,
		},
		{
			title: 'Import deck file',
			ctrl: true, key: 'i',
			action: () => editorReady.value && !modalsOpen.value ?
				state.editor.modals.importer = true : void 0,
		},
		{
			title: 'Export deck file',
			ctrl: true, key: 'e',
			action: () => editorReady.value && !modalsOpen.value ?
				state.editor.modals.exporter = true : void 0,
		},
		{
			title: 'Show deck version history',
			ctrl: true, key: 'h',
			action: () => editorReady.value && !modalsOpen.value && deckPublished.value ?
				state.editor.modals.versions = true : void 0,
		},
		{
			title: 'Play deck',
			ctrl: true, key: 'p',
			action: () => editorReady.value && deckPublished.value ?
				openPlayView() : void 0,
		},
		{
			title: 'Focus the front card side',
			ctrl: true, key: 'arrowleft',
			prepreq: () => !isInteractive(document.activeElement),
			action: () => state.editor.view.side = EditedSide.Front,
		},
		{
			title: 'Focus the back card side',
			ctrl: true, key: 'arrowright',
			prepreq: () => !isInteractive(document.activeElement),
			action: () => state.editor.view.side = EditedSide.Back,
		},
		{
			title: 'Clear card side focus',
			key: 'escape',
			prepreq: () => !isInteractive(document.activeElement) && !!state.editor.view.side,
			action: () => {
				if (document.activeElement instanceof HTMLElement) {
					document.activeElement.blur();
				}
				state.editor.view.side = null;
			},
		},
		{
			title: 'Previous card',
			ctrl: true, key: 'arrowup',
			prepreq: () => !isInteractive(document.activeElement),
			action: () => {
				state.editor.view.side = null;
				state.editor.view.cardIdx = Math.max(0, state.editor.view.cardIdx - 1);
			},
		},
		{
			title: 'Next card',
			ctrl: true, key: 'arrowdown',
			prepreq: () => !isInteractive(document.activeElement),
			action: () => {
				state.editor.view.side = null;
				state.editor.view.cardIdx = Math.min(state.editor.view.cardIdx + 1, state.content.cards.length - 1);
			},
		},
		{
			title: 'Undo change',
			ctrl: true, key: 'z',
			prepreq: () => editorReady.value && !isInteractive(document.activeElement) && historyCanUndo.value,
			action: editorHistoryBack,
		},
		{
			title: 'Redo change',
			ctrl: true, key: 'y',
			prepreq: () => editorReady.value && !isInteractive(document.activeElement) && historyCanRedo.value,
			action: editorHistoryForward,
		},
	]);
	state.editor.shortcuts.register();
};

onMounted(async () => {

	watch(() => state.content.meta.summary.name, updateAppTitle, { immediate: true });

	watchContentEdits();
	registerShortcuts();

	const { deck_id } = route.params;
	if (typeof deck_id === 'string') {

		state.origin.deckID = deck_id;

		await restoreStateSnapshot().catch(error => console.error('restoreStateSnapshot', error));

		if (!state.editor.snapshots.loadedVersion) {
			await fetchRemoteState(deck_id);
		}

		state.editor.ready = !state.editor.error;
		return;
	}

	const { collection_id } = Object.fromEntries(new URLSearchParams(window.location.search).entries());
	if (collection_id) {

		await restoreStateSnapshot().catch(error => console.error('restoreStateSnapshot', error));

		state.origin.deckID = null;
		state.origin.collectionID = collection_id;

		state.editor.ready = true;
		return;
	}

	state.editor.error = 'Invalid editor URL';
});

onUnmounted(() => {
	state.editor.shortcuts?.unregister();
});

</script>

<template>

	<div class="deck-editor">

		<EditorScreenOverlay v-if="!state.editor.ready || state.editor.error">

			<OverlayErrorMessage v-if="state.editor.error" :backHref="backHref">

				Unable to load editor

				<template v-slot:details>
					{{ state.editor.error }}
				</template>

				<template v-slot:after>
					<GenericButton variant="thin" @click="clearAndExitEditor">
						Go back
					</GenericButton>
				</template>

			</OverlayErrorMessage>

			<template v-else>
				<LoadingMessage>
					Loading editor ...
				</LoadingMessage>
			</template>

		</EditorScreenOverlay>

		<DeckEditorHeader @exit="saveAndExitEditor">

			<template v-slot:meta>
				<DeckEditorSummary
					:meta="state.content.meta"
					:changed="contentEdited"
					:changesSaved="changesSaved" />
			</template>

			<template v-slot:autosave>
				<DeckEditorAutosaveIndicator :changed="contentEdited" :changesSaved="changesSaved" />
			</template>

			<template v-slot:publish>

				<GenericButton
					variant="thin"
					:theme="deckPublished ? 'blue' : 'green'"
					:disabled="!editorReady"
					@click="state.editor.modals.publish = true">

					<template v-if="!deckPublished">
						Publish
					</template>

					<template v-else>
						Update
					</template>

				</GenericButton>

			</template>

			<template v-slot:colors>

				<DeckEditorElementColorDropdown label="Card fill color" icon="fill"
					:disabled="!editedCardFace"
					v-bind:modelValue="editedCardFace?.theme?.card?.fill_color"
					@update:modelValue="fill_color => handleCardCanvasThemeUpdate({ fill_color })" />

				<DeckEditorElementColorDropdown label="Text color" icon="text"
					:disabled="!editedCardFace"
					v-bind:modelValue="editedCardFace?.theme?.card?.mask_color"
					@update:modelValue="mask_color => handleCardCanvasThemeUpdate({ mask_color })" />

				<DeckEditorElementColorDropdown label="Outline color" icon="outline"
					:disabled="!editedCardFace"
					v-bind:modelValue="editedCardFace?.theme?.card?.outline_color"
					@update:modelValue="outline_color => handleCardCanvasThemeUpdate({ outline_color })" />

				<DeckEditorElementColorDropdown label="Interactive color" icon="fill-interactive"
					:disabled="!editedCardFace"
					v-bind:modelValue="editedCardFace?.theme?.interactives?.fill_color"
					@update:modelValue="fill_color => handleCardContentElementThemeUpdate({ fill_color })" />

				<DeckEditorElementColorDropdown label="Interactive mask color" icon="text-interactive"
					:disabled="!editedCardFace"
					v-bind:modelValue="editedCardFace?.theme?.interactives?.mask_color"
					@update:modelValue="mask_color => handleCardContentElementThemeUpdate({ mask_color })" />

			</template>

			<template v-slot:insert>

				<DeckEditorElementButton label="Add title" icon="add-title"
					:disabled="!insertableContentNodes.title"
					@click="addModelNode(editedCardFace?.content, 'title')" />

				<DeckEditorElementButton label="Add text box" icon="add-text"
					:disabled="!insertableContentNodes.textbox"
					@click="addModelNode(editedCardFace?.content, 'textbox')" />

				<DeckEditorElementButton label="Add image" icon="add-image"
					:disabled="!insertableContentNodes.image"
					@click="addModelNode(editedCardFace?.content, 'image')" />

				<DeckEditorElementButton label="Add poll" icon="add-poll"
					:disabled="!insertableContentNodes.poll"
					@click="addModelNode(editedCardFace?.content, 'poll')" />

			</template>

			<template v-slot:ribbon>

				<DeckEditorMenu label="Deck">

					<DeckEditorMenuEntry label="Play deck" icon="play"
						:disabled="!editorReady || !deckPublished" @click="openPlayView" />

					<DeckEditorMenuEntry label="Details" icon="info"
						:disabled="!editorReady" @click="state.editor.modals.details = true" />

					<DeckEditorMenuEntry label="Versions" icon="history"
						:disabled="!editorReady || !deckPublished" @click="state.editor.modals.versions = true" />

					<DeckEditorMenuEntry label="Import deck" icon="io"
						:disabled="!editorReady" @click="state.editor.modals.importer = true" />

					<DeckEditorMenuEntry label="Export deck" icon="io"
						:disabled="!editorReady" @click="state.editor.modals.exporter = true" />

					<DeckEditorMenuEntry label="Publish/update" icon="publish"
						:disabled="!editorReady" @click="state.editor.modals.publish = true" />

					<DeckEditorMenuEntry label="Duplicate deck" icon="copy"
						:disabled="!editorReady || !deckPublished" @click="duplicateDeck" />

					<DeckEditorMenuEntry label="Discard all and exit" icon="cross"
						:disabled="!editorReady" @click="clearAndExitEditorPrompt" />

					<DeckEditorMenuEntry label="Delete deck" icon="delete"
						:disabled="!editorReady || !deckPublished" @click="deleteDeckAndExit" />

					<DeckEditorMenuEntry label="Exit" icon="exit"
						@click="saveAndExitEditor" />

				</DeckEditorMenu>

				<DeckEditorMenu label="Changes">

					<DeckEditorMenuEntry label="Undo" icon="undo"
						:disabled="!editorReady || !historyCanUndo"
						@click="editorHistoryBack" />

					<DeckEditorMenuEntry label="Redo" icon="redo"
						:disabled="!editorReady || !historyCanRedo"
						@click="editorHistoryForward" />

					<DeckEditorMenuEntry label="Discard local changes" icon="broom"
						:disabled="!editorReady || !contentEdited || !deckPublished" @click="dropLocalChanges" />

				</DeckEditorMenu>

				<DeckEditorMenu label="Select">

					<DeckEditorMenuEntry label="Select front side"
						:disabled="!editedCardNode || state.editor.view.side === EditedSide.Front"
						@click="state.editor.view.side = EditedSide.Front" />

					<DeckEditorMenuEntry label="Select back side"
						:disabled="!editedCardNode || state.editor.view.side === EditedSide.Back"
						@click="state.editor.view.side = EditedSide.Back" />

					<DeckEditorMenuEntry label="Deselect all"
						:disabled="!editedCardNode || !state.editor.view.side"
						@click="state.editor.view.side = null" />

				</DeckEditorMenu>

				<DeckEditorMenu label="Insert">

					<DeckEditorMenuEntry label="Insert title" icon="add-title"
						:disabled="!insertableContentNodes.title"
						@click="addModelNode(editedCardFace?.content, 'title')" />

					<DeckEditorMenuEntry label="Insert text area" icon="add-text"
						:disabled="!insertableContentNodes.textbox"
						@click="addModelNode(editedCardFace?.content, 'textbox')" />

					<DeckEditorMenuEntry label="Insert image" icon="add-image"
						:disabled="!insertableContentNodes.image"
						@click="addModelNode(editedCardFace?.content, 'image')" />

					<DeckEditorMenuEntry label="Insert poll" icon="add-poll"
						:disabled="!insertableContentNodes.poll"
						@click="addModelNode(editedCardFace?.content, 'poll')" />

				</DeckEditorMenu>

				<DeckEditorMenu label="Help">
					<DeckEditorMenuEntry label="Keyboard shortcuts" icon="keyboard" @click="state.editor.modals.shortcuts = true" />
				</DeckEditorMenu>

			</template>

		</DeckEditorHeader>

		<EditorScreenOverlay v-if="state.editor.modals.exporter">
			<EditorContentExporter
				:meta="state.origin"
				:content="state.content"
				@done="state.editor.modals.exporter = false" />
		</EditorScreenOverlay>

		<EditorScreenOverlay v-else-if="state.editor.modals.importer">
			<EditorContentImporter
				@addCards="cards => state.content.cards.push(...cards)"
				@replaceCards="cards => state.content.cards = cards"
				@updateMeta="patchDeckSummary"
				@done="state.editor.modals.importer = false" />
		</EditorScreenOverlay>

		<EditorScreenOverlay v-else-if="state.editor.modals.versions && state.origin.deckID">
			<EditorVersionControlModal
				:deckID="state.origin.deckID"
				@pull="applyPulledVersion"
				@done="state.editor.modals.versions = false" />
		</EditorScreenOverlay>

		<EditorScreenOverlay v-else-if="state.editor.modals.publish">
			<EditorVersionPublishModal
				:origin="state.origin"
				:content="state.content"
				:changes="state.editor.changes"
				@publish="applyPublishedMeta"
				@done="state.editor.modals.publish = false" />
		</EditorScreenOverlay>

		<EditorScreenOverlay v-else-if="state.editor.modals.details">
			<EditorDeckDetailsModal :content="state.content" :origin="state.origin" @done="state.editor.modals.details = false" />
		</EditorScreenOverlay>

		<EditorScreenOverlay v-else-if="state.editor.modals.shortcuts">
			<EditorShortcutsModal :shortcuts="state.editor.shortcuts?.entries()" @done="state.editor.modals.shortcuts = false" />
		</EditorScreenOverlay>

		<div class="deck-editor-canvas">

			<div class="canvas-grid">

				<DeckCardList
					:list="cardSelectorList"
					:pointer="state.editor.view.cardIdx"
					@select="selectCard"
					@add="createCard"
					@duplicate="duplicateCard"
					@moveUp="idx => reorderCard(idx, -1)"
					@moveDown="idx => reorderCard(idx, 1)"
					@remove="removeCard" />

				<CardFaceEditor
					v-model="editedCardNode.front"
					:isFront="true"
					:active="state.editor.view.side === EditedSide.Front"
					@click="state.editor.view.side = EditedSide.Front" />

				<hr />

				<CardFaceEditor
					v-model="editedCardNode.back"
					:active="state.editor.view.side === EditedSide.Back"
					@click="state.editor.view.side = EditedSide.Back" />

			</div>
		</div>

	</div>

</template>

<style lang="scss" scoped>
	.deck-editor {
		display: flex;
		flex-direction: column;
		gap: 2rem;
		width: 100%;
		height: 100%;
		position: relative;

		.deck-editor-canvas {
			display: flex;
			justify-content: center;
			width: 100%;
			height: 100%;
			min-height: 0;

			.canvas-grid {
				display: grid;
				grid-template-columns: auto 1fr 1px 1fr;
				gap: 1rem;
				width: 100%;
				max-width: 70rem;
				height: 100%;
			}

			hr {
				display: block;
				background-color: var(--app-theme-powder-trail);
				width: 1px;
				height: 100%;
				outline: none;
				border: none;
			}
		}
	}
</style>
