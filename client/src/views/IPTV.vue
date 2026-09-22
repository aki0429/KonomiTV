<template>
    <div class="route-container">
        <HeaderBar />
        <main>
            <Navigation />
            <div class="iptv-container-wrapper">
                <SPHeaderBar />
                <div class="iptv-container">
                    <Breadcrumbs :crumbs="breadcrumbs" />
                    <!-- ヘッダー: タイトル・件数・操作 -->
                    <div class="iptv__header">
                        <h2 class="iptv__title">
                            <span class="iptv__title-text">IPTV</span>
                            <div class="iptv__title-count" v-if="total > 0">{{ total }}チャンネル</div>
                            <div class="iptv__title-updated">最終更新: {{ updated_at_text }}</div>
                        </h2>
                        <div class="iptv__actions">
                            <v-btn class="iptv__action-button" variant="tonal" color="primary"
                                :loading="is_loading" @click="refreshAll()">
                                <Icon icon="fluent:arrow-sync-20-regular" width="18px" class="mr-1" />
                                再取得
                            </v-btn>
                            <v-btn class="iptv__action-button" variant="tonal" @click="openSourceDialog()">
                                <Icon icon="fluent:settings-20-regular" width="18px" class="mr-1" />
                                ソース管理
                            </v-btn>
                        </div>
                    </div>
                    <!-- 国・検索の絞り込み -->
                    <div class="iptv__filters">
                        <div class="iptv__filter-row">
                            <v-select class="iptv__country-select" color="primary" bg-color="background-lighten-1"
                                variant="solo" density="comfortable" hide-details
                                :items="country_items" item-title="title" item-value="value"
                                v-model="selected_country" @update:model-value="onCountryChange()">
                                <template #item="{ props: item_props, item }">
                                    <v-list-item v-bind="item_props" :title="item.raw.title"></v-list-item>
                                </template>
                            </v-select>
                            <v-text-field class="iptv__search" color="primary" bg-color="background-lighten-1"
                                variant="solo" density="comfortable" hide-details clearable
                                prepend-inner-icon="fluent:search-20-regular" placeholder="チャンネル名で検索"
                                v-model="search_query" @keydown.enter="onSearchChange()"
                                @click:clear="onSearchChange()">
                            </v-text-field>
                        </div>
                        <!-- グループフィルタ (選択中の国のグループのみ) -->
                        <div class="iptv__group-chips" v-if="groups.length > 0">
                            <v-chip class="iptv__chip" variant="elevated"
                                :color="selected_group === null ? 'primary' : undefined"
                                @click="onGroupChange(null)">
                                すべて
                            </v-chip>
                            <v-chip class="iptv__chip" variant="elevated" v-for="group in groups" :key="group.name"
                                :color="selected_group === group.name ? 'primary' : undefined"
                                @click="onGroupChange(group.name)">
                                {{ group.name }}
                                <span class="iptv__chip-count">{{ group.count }}</span>
                            </v-chip>
                        </div>
                    </div>
                    <!-- 取得エラーがある場合の警告 -->
                    <v-alert class="iptv__alert" v-if="source_errors.length > 0" type="warning" variant="tonal" density="compact">
                        <div>{{ source_errors.length }} 件のプレイリストを取得できませんでした。</div>
                        <div class="iptv__alert-detail" v-for="error in source_errors" :key="error.url">
                            {{ error.url }} ({{ error.message }})
                        </div>
                    </v-alert>
                    <!-- チャンネルグリッド -->
                    <div class="iptv__grid" :class="{'iptv__grid--empty': channels.length === 0 && !is_loading}">
                        <!-- ローディング中 -->
                        <div class="iptv__loading" v-if="is_loading">
                            <v-progress-circular indeterminate color="primary" size="48"></v-progress-circular>
                            <div class="iptv__loading-text">プレイリストを取得しています…</div>
                        </div>
                        <!-- 空状態 -->
                        <div class="iptv__empty" v-else-if="channels.length === 0">
                            <Icon class="iptv__empty-icon" icon="fluent:tv-20-regular" width="54px" height="54px" />
                            <h2>{{ all_total === 0 ? 'チャンネルがありません' : '条件に一致するチャンネルがありません' }}</h2>
                            <div class="iptv__empty-submessage">
                                {{ all_total === 0 ? 'ソース管理から M3U プレイリストを追加してください。' : '検索条件や国を変更してください。' }}
                            </div>
                        </div>
                        <!-- チャンネルカード -->
                        <div class="iptv-channel-card" v-for="channel in channels" :key="channel.id"
                            @click="playChannel(channel)">
                            <div class="iptv-channel-card__logo">
                                <img v-if="channel.logo_url !== null && logo_errors.has(channel.id) === false"
                                    :src="IPTV.getLogoURL(channel.logo_url) ?? ''" :alt="channel.name" loading="lazy"
                                    @error="logo_errors.add(channel.id)">
                                <Icon v-else icon="fluent:tv-20-regular" width="40px" height="40px" />
                            </div>
                            <div class="iptv-channel-card__info">
                                <div class="iptv-channel-card__name">
                                    <span v-if="channel.country_flag" class="iptv-channel-card__flag">{{ channel.country_flag }}</span>
                                    {{ channel.name }}
                                </div>
                                <div class="iptv-channel-card__meta">
                                    <span v-if="channel.group !== null" class="iptv-channel-card__group">{{ channel.group }}</span>
                                    <span v-if="channel.is_geo_blocked" class="iptv-channel-card__badge">地域制限</span>
                                    <span v-if="channel.stream_type === 'DASH' || channel.stream_type === 'Other'"
                                        class="iptv-channel-card__badge">{{ channel.stream_type }}</span>
                                </div>
                            </div>
                        </div>
                    </div>
                    <!-- ページネーション -->
                    <div class="iptv__pagination" v-if="max_page > 1">
                        <v-pagination color="primary" :length="max_page" :total-visible="7"
                            v-model="page" @update:model-value="onPageChange()">
                        </v-pagination>
                    </div>
                </div>
            </div>
        </main>
        <!-- プレイヤーダイアログ -->
        <v-dialog class="iptv-player-dialog" v-model="player_dialog_open" max-width="1024" scrollable
            @after-enter="onPlayerDialogAfterEnter" @keydown.esc="closePlayer()">
            <v-card class="iptv-player" color="background-lighten-1">
                <v-card-title class="iptv-player__header">
                    <div class="iptv-player__channel">
                        <div class="iptv-player__channel-name">{{ playing_channel?.name ?? '' }}</div>
                        <div class="iptv-player__channel-group">
                            {{ playing_channel?.country_flag ?? '' }} {{ playing_channel?.country_name ?? '' }}
                            <span v-if="playing_channel?.group"> / {{ playing_channel?.group }}</span>
                        </div>
                    </div>
                    <v-btn icon variant="text" @click="closePlayer()">
                        <Icon icon="fluent:dismiss-20-regular" width="22px" />
                    </v-btn>
                </v-card-title>
                <v-card-text class="iptv-player__body">
                    <div class="iptv-player__video-wrapper">
                        <video class="iptv-player__video" ref="video_element" controls autoplay playsinline></video>
                        <!-- ローディング中 -->
                        <div class="iptv-player__overlay" v-if="player_status === 'loading'">
                            <v-progress-circular indeterminate color="primary" size="44"></v-progress-circular>
                            <div class="iptv-player__overlay-text">ストリームに接続しています…</div>
                        </div>
                        <!-- 再生エラー -->
                        <div class="iptv-player__overlay iptv-player__overlay--error" v-else-if="player_status === 'error'">
                            <Icon icon="fluent:warning-20-regular" width="44px" height="44px" />
                            <div class="iptv-player__overlay-text">{{ player_error }}</div>
                        </div>
                    </div>
                </v-card-text>
            </v-card>
        </v-dialog>
        <!-- ソース管理ダイアログ -->
        <v-dialog class="iptv-source-dialog" v-model="source_dialog_open" max-width="760" scrollable>
            <v-card class="iptv-source" color="background-lighten-1">
                <v-card-title class="iptv-source__header">
                    <div class="iptv-source__title">M3U プレイリストのソース</div>
                    <v-btn icon variant="text" @click="source_dialog_open = false">
                        <Icon icon="fluent:dismiss-20-regular" width="22px" />
                    </v-btn>
                </v-card-title>
                <v-card-text class="iptv-source__body">
                    <div class="iptv-source__section-title">追加登録したソース</div>
                    <div class="iptv-source__list" v-if="sources !== null && sources.user_sources.length > 0">
                        <div class="iptv-source__item" v-for="source in sources.user_sources" :key="source">
                            <span class="iptv-source__item-url">{{ source }}</span>
                            <v-btn icon variant="text" size="small" :loading="is_removing_source"
                                @click="removeSource(source)">
                                <Icon icon="fluent:delete-20-regular" width="18px" />
                            </v-btn>
                        </div>
                    </div>
                    <div class="iptv-source__empty" v-else>追加登録したソースはありません。</div>
                    <div class="iptv-source__section-title">ソースを追加</div>
                    <div class="iptv-source__add">
                        <v-text-field class="iptv-source__add-input" color="primary" bg-color="background-lighten-1"
                            variant="solo" density="comfortable" hide-details
                            placeholder="https://example.com/playlist.m3u" v-model="new_source_url"
                            @keydown.enter="addSource()">
                        </v-text-field>
                        <v-btn class="iptv-source__add-button" color="primary" variant="flat"
                            :loading="is_adding_source" :disabled="new_source_url.trim() === ''" @click="addSource()">
                            追加
                        </v-btn>
                    </div>
                    <div class="iptv-source__section-title">config.yaml で設定されたソース (変更するには config.yaml を編集してください)</div>
                    <div class="iptv-source__list">
                        <div class="iptv-source__item iptv-source__item--readonly" v-for="source in sources?.config_sources ?? []" :key="source">
                            <span class="iptv-source__item-url">{{ source }}</span>
                        </div>
                    </div>
                </v-card-text>
            </v-card>
        </v-dialog>
    </div>
</template>
<script lang="ts" setup>

import Hls from 'hls.js';
import mpegts from 'mpegts.js';
import { computed, nextTick, onBeforeUnmount, onMounted, ref } from 'vue';

import Breadcrumbs from '@/components/Breadcrumbs.vue';
import HeaderBar from '@/components/HeaderBar.vue';
import Navigation from '@/components/Navigation.vue';
import SPHeaderBar from '@/components/SPHeaderBar.vue';
import Message from '@/message';
import IPTV, { IIPTVChannel, IIPTVCountry, IIPTVGroup, IIPTVSources } from '@/services/IPTV';
import { dayjs } from '@/utils';

// パンくずリスト
const breadcrumbs = [
    { name: 'ホーム', path: '/' },
    { name: 'IPTV', path: '/iptv/', disabled: true },
];

// 1ページあたりの表示件数
const PER_PAGE = 60;

// ==================== チャンネル一覧の状態 ====================

const channels = ref<IIPTVChannel[]>([]);
const total = ref(0);
const all_total = ref(0);
const max_page = ref(1);
const page = ref(1);
const updated_at = ref<number | null>(null);
const is_loading = ref(true);

// プレイリストの取得に失敗したソース (URL とエラーメッセージ)
const source_errors = ref<{url: string; message: string}[]>([]);

// ロゴの読み込みに失敗したチャンネル ID のセット
const logo_errors = ref<Set<string>>(new Set());

// ==================== 絞り込みの状態 ====================

const countries = ref<IIPTVCountry[]>([]);
const selected_country = ref<string | null>(null);
const groups = ref<IIPTVGroup[]>([]);
const selected_group = ref<string | null>(null);
const search_query = ref('');

// 国選択の選択肢 (先頭に「すべての国」を追加する)
const country_items = computed(() => {
    const items = [{ title: `すべての国 (${all_total.value})`, value: null as string | null }];
    for (const country of countries.value) {
        items.push({
            title: `${country.flag} ${country.name} (${country.count})`,
            value: country.code,
        });
    }
    return items;
});

// 最終更新日時 (表示用)
const updated_at_text = computed(() => {
    if (updated_at.value === null) {
        return '未取得';
    }
    return dayjs(new Date(updated_at.value * 1000)).format('YYYY/MM/DD HH:mm:ss');
});

// ==================== プレイヤーの状態 ====================

const player_dialog_open = ref(false);
const playing_channel = ref<IIPTVChannel | null>(null);
const player_status = ref<'loading' | 'playing' | 'error'>('loading');
const player_error = ref('');
const video_element = ref<HTMLVideoElement | null>(null);

// 再生中の HLS / MPEG-TS のインスタンス (破棄用に保持する)
let hls_instance: Hls | null = null;
let mpegts_instance: mpegts.Player | null = null;
// HLS の致命的エラーからの復帰を試行した回数
let hls_recovery_count = 0;
// MPEG-TS での再生へフォールバック済みかどうか
let tried_mpegts_fallback = false;
// 現在のチャンネルの再生を開始済みかどうか (nextTick と after-enter の両方から呼ばれるため、二重起動を防ぐ)
let playback_started = false;

// ==================== ソース管理の状態 ====================

const source_dialog_open = ref(false);
const sources = ref<IIPTVSources | null>(null);
const new_source_url = ref('');
const is_adding_source = ref(false);
const is_removing_source = ref(false);

// ==================== データ取得 ====================

/**
 * チャンネル一覧を取得する
 * @param force_refresh キャッシュを無視してプレイリストを再取得するかどうか
 */
async function fetchChannels(force_refresh: boolean = false): Promise<void> {

    is_loading.value = true;
    try {
        const result = await IPTV.fetchChannels({
            country: selected_country.value,
            group: selected_group.value,
            search: search_query.value.trim() === '' ? null : search_query.value.trim(),
            page: page.value,
            per_page: PER_PAGE,
            refresh: force_refresh,
        });
        if (result === null) {
            return;
        }
        channels.value = result.channels;
        total.value = result.total;
        all_total.value = result.all_total;
        max_page.value = result.max_page;
        page.value = result.page;
        updated_at.value = result.updated_at;
        // 取得に失敗したソースを警告表示用に整形する
        source_errors.value = Object.entries(result.errors).map(([url, message]) => ({url, message}));
        // 再取得時はロゴの読み込み失敗状態をリセットする
        logo_errors.value = new Set();
    } finally {
        is_loading.value = false;
    }
}

/**
 * 国一覧を取得する
 */
async function fetchCountries(): Promise<void> {

    const result = await IPTV.fetchCountries();
    if (result === null) {
        return;
    }
    countries.value = result.countries;
    all_total.value = result.all_total;
    if (updated_at.value === null) {
        updated_at.value = result.updated_at;
    }
}

/**
 * 選択中の国に含まれるグループ一覧を取得する
 */
async function fetchGroups(): Promise<void> {

    const result = await IPTV.fetchGroups(selected_country.value);
    if (result === null) {
        return;
    }
    groups.value = result.groups;
    // 選択中のグループが新しい一覧に存在しない場合は解除する
    if (selected_group.value !== null && groups.value.some((group) => group.name === selected_group.value) === false) {
        selected_group.value = null;
    }
}

/**
 * 国を変更した際の処理
 */
async function onCountryChange(): Promise<void> {

    // 国を切り替えたらグループとページをリセットする
    selected_group.value = null;
    page.value = 1;
    await fetchGroups();
    await fetchChannels();
}

/**
 * グループを変更した際の処理
 * @param group 選択されたグループ名 (null の場合はすべて)
 */
async function onGroupChange(group: string | null): Promise<void> {

    selected_group.value = group;
    page.value = 1;
    await fetchChannels();
}

/**
 * 検索キーワードを変更した際の処理
 */
async function onSearchChange(): Promise<void> {

    page.value = 1;
    await fetchChannels();
}

/**
 * ページを変更した際の処理
 */
async function onPageChange(): Promise<void> {

    await fetchChannels();
    // ページ切り替え時は一覧の先頭へスクロールする
    window.scrollTo({top: 0, behavior: 'smooth'});
}

/**
 * プレイリストと国一覧を強制的に再取得する
 */
async function refreshAll(): Promise<void> {

    await fetchChannels(true);
    await fetchCountries();
    await fetchGroups();
    Message.success('IPTV のプレイリストを再取得しました。');
}

// ==================== ソース管理 ====================

/**
 * ソース管理ダイアログを開き、ソース一覧を取得する
 */
async function openSourceDialog(): Promise<void> {

    source_dialog_open.value = true;
    sources.value = await IPTV.fetchSources();
}

/**
 * 入力された URL をソースに追加する
 */
async function addSource(): Promise<void> {

    const url = new_source_url.value.trim();
    if (url === '' || is_adding_source.value === true) {
        return;
    }

    is_adding_source.value = true;
    try {
        const result = await IPTV.addSource(url);
        if (result === null) {
            return;
        }
        sources.value = result;
        new_source_url.value = '';
        Message.success('IPTV のソースを追加しました。');
        // 追加したソースを反映する
        page.value = 1;
        await fetchChannels(true);
        await fetchCountries();
        await fetchGroups();
    } finally {
        is_adding_source.value = false;
    }
}

/**
 * 追加登録したソースを削除する
 * @param url 削除するソースの URL
 */
async function removeSource(url: string): Promise<void> {

    if (is_removing_source.value === true) {
        return;
    }

    is_removing_source.value = true;
    try {
        const result = await IPTV.removeSource(url);
        if (result === null) {
            return;
        }
        sources.value = result;
        Message.success('IPTV のソースを削除しました。');
        page.value = 1;
        await fetchChannels(true);
        await fetchCountries();
        await fetchGroups();
    } finally {
        is_removing_source.value = false;
    }
}

// ==================== プレイヤー ====================

/**
 * 現在の再生を停止し、関連するインスタンスを破棄する
 */
function stopPlayback(): void {

    // HLS のインスタンスを破棄
    if (hls_instance !== null) {
        hls_instance.destroy();
        hls_instance = null;
    }
    // MPEG-TS のインスタンスを破棄
    if (mpegts_instance !== null) {
        mpegts_instance.destroy();
        mpegts_instance = null;
    }
    // video 要素の再生を停止し、ソースを解放する
    const video = video_element.value;
    if (video !== null) {
        video.pause();
        video.removeAttribute('src');
        video.load();
    }
    // フォールバックの状態をリセット
    hls_recovery_count = 0;
    tried_mpegts_fallback = false;
    playback_started = false;
}

/**
 * 再生に失敗した際の共通処理
 * @param message エラーメッセージ
 */
function failPlayback(message: string): void {
    player_status.value = 'error';
    player_error.value = message;
}

/**
 * HLS で再生を開始する
 * @param url ストリームの URL
 */
function startHLS(url: string): void {

    const video = video_element.value;
    if (video === null) {
        return;
    }

    // hls.js が使えない (Safari など) 場合は、ネイティブの HLS 再生を試す
    if (Hls.isSupported() === false) {
        startNative(url);
        return;
    }

    hls_instance = new Hls({
        enableWorker: true,
        lowLatencyMode: false,
        // メモリ使用量を抑えるため、バックバッファを短めにする
        backBufferLength: 60,
    });

    // マニフェストの読み込みが完了したら再生を開始する
    hls_instance.on(Hls.Events.MANIFEST_PARSED, () => {
        player_status.value = 'playing';
        video.play().catch(() => {
            // 自動再生がブロックされた場合はエラーにせず、ユーザー操作での再生を待つ
        });
    });

    // エラー発生時のハンドリング
    hls_instance.on(Hls.Events.ERROR, (_event, data) => {
        if (data.fatal === false) {
            return;
        }
        // ネットワークエラーは一度だけ復帰を試みる
        if (data.type === Hls.ErrorTypes.NETWORK_ERROR && hls_recovery_count < 1) {
            hls_recovery_count += 1;
            hls_instance?.startLoad();
            return;
        }
        // メディアエラーは一度だけ復帰を試みる
        if (data.type === Hls.ErrorTypes.MEDIA_ERROR && hls_recovery_count < 1) {
            hls_recovery_count += 1;
            hls_instance?.recoverMediaError();
            return;
        }
        // それでも復帰できない場合
        if (hls_instance !== null) {
            hls_instance.destroy();
            hls_instance = null;
        }
        // フォーマットが不明なストリームの場合は、MPEG-TS として再生を試す
        if (tried_mpegts_fallback === false && playing_channel.value !== null && playing_channel.value.stream_type === 'Other') {
            tried_mpegts_fallback = true;
            startMPEGTS(url);
            return;
        }
        failPlayback('このストリームは再生できませんでした。配信サーバーが停止しているか、再生に対応していない形式の可能性があります。');
    });

    hls_instance.loadSource(url);
    hls_instance.attachMedia(video);
}

/**
 * MPEG-TS で再生を開始する
 * @param url ストリームの URL
 */
function startMPEGTS(url: string): void {

    const video = video_element.value;
    if (video === null) {
        return;
    }

    // mpegts.js が使えない場合はネイティブ再生を試す
    if (mpegts.isSupported() === false) {
        startNative(url);
        return;
    }

    mpegts_instance = mpegts.createPlayer({
        type: 'mpegts',
        isLive: true,
        url: url,
    }, {
        enableWorker: true,
        liveBufferLatencyChasing: true,
    });

    mpegts_instance.on(mpegts.Events.ERROR, (error_type, error_detail) => {
        // 再生に失敗した場合はエラー表示にする
        if (mpegts_instance !== null) {
            mpegts_instance.destroy();
            mpegts_instance = null;
        }
        failPlayback(`このストリームは再生できませんでした。(${error_type} / ${error_detail})`);
    });

    mpegts_instance.attachMediaElement(video);
    mpegts_instance.load();
    player_status.value = 'playing';
    const play_result = mpegts_instance.play();
    // 自動再生がブロックされた場合はエラーにせず、ユーザー操作での再生を待つ
    if (play_result instanceof Promise) {
        play_result.catch(() => {});
    }
}

/**
 * ネイティブの video 要素で再生を開始する (MP4 や Safari の HLS 向け)
 * @param url ストリームの URL
 */
function startNative(url: string): void {

    const video = video_element.value;
    if (video === null) {
        return;
    }

    video.src = url;
    video.onloadedmetadata = () => {
        player_status.value = 'playing';
    };
    video.onerror = () => {
        failPlayback('このストリームは再生できませんでした。');
    };
    video.play().catch(() => {
        // 自動再生がブロックされた場合はエラーにせず、ユーザー操作での再生を待つ
    });
}

/**
 * 現在選択されているチャンネルの再生を、配信フォーマットに応じて開始する
 * playChannel() と onPlayerDialogAfterEnter() の両方から呼ばれる可能性があるため、二重起動を防ぐ
 */
function startPlaybackForCurrentChannel(): void {

    const channel = playing_channel.value;
    if (channel === null || playback_started === true) {
        return;
    }
    playback_started = true;

    const url = IPTV.getStreamURL(channel.stream_url);
    switch (channel.stream_type) {
        case 'MPEGTS': {
            startMPEGTS(url);
            break;
        }
        case 'MP4': {
            startNative(url);
            break;
        }
        case 'DASH': {
            // DASH は hls.js / mpegts.js では再生できない (Safari などのネイティブ再生にのみ対応)
            startNative(url);
            break;
        }
        default: {
            // HLS と判定されたストリーム、および判定できないストリームは HLS として再生を試みる
            startHLS(url);
            break;
        }
    }
}

/**
 * 指定されたチャンネルの再生を開始する
 * @param channel 再生するチャンネル
 */
async function playChannel(channel: IIPTVChannel): Promise<void> {

    // 直前の再生を停止する
    stopPlayback();

    playing_channel.value = channel;
    player_status.value = 'loading';
    player_error.value = '';
    player_dialog_open.value = true;

    // video 要素が描画されるまで待ってから再生を開始する
    // 遷移アニメーションが無効な環境では after-enter が発火しないことがあるため、こちらを主とする
    await nextTick();
    startPlaybackForCurrentChannel();
}

/**
 * プレイヤーダイアログが開ききった際に呼ばれる
 * nextTick で再生が始まっていない場合のフォールバックとして再生を開始する
 */
function onPlayerDialogAfterEnter(): void {

    if (player_dialog_open.value === true) {
        startPlaybackForCurrentChannel();
    }
}

/**
 * プレイヤーを閉じる
 */
function closePlayer(): void {
    stopPlayback();
    player_dialog_open.value = false;
    playing_channel.value = null;
}

// ==================== ライフサイクル ====================

onMounted(async () => {
    await fetchCountries();
    await fetchGroups();
    await fetchChannels();
});

onBeforeUnmount(() => {
    // 画面を離れる際は必ず再生を停止する
    stopPlayback();
});

</script>
<style lang="scss" scoped>

.iptv-container-wrapper {
    display: flex;
    flex-direction: column;
    flex-grow: 1;
    min-width: 0;
}

.iptv-container {
    box-sizing: border-box;
    width: 100%;
    max-width: 1800px;
    margin: 0 auto;
    padding: 24px 32px 48px;
    @include smartphone-vertical {
        padding: 12px 12px 32px;
    }
    @include smartphone-horizontal {
        padding: 12px 16px 32px;
    }
}

.iptv__header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
    margin-bottom: 16px;
}

.iptv__title {
    display: flex;
    align-items: center;
    gap: 12px;
    margin: 0;
    font-size: 24px;
    font-weight: 600;
    white-space: nowrap;
}

.iptv__title-count {
    padding: 2px 10px;
    border-radius: 999px;
    background: rgb(var(--v-theme-background-lighten-2));
    font-size: 12px;
    font-weight: 500;
    color: rgb(var(--v-theme-gray));
}

.iptv__title-updated {
    font-size: 12px;
    font-weight: 400;
    color: rgb(var(--v-theme-gray));
    white-space: nowrap;
    @include smartphone-vertical {
        display: none;
    }
}

.iptv__actions {
    display: flex;
    flex-shrink: 0;
    gap: 8px;
}

.iptv__filters {
    display: flex;
    flex-direction: column;
    gap: 12px;
    margin-bottom: 16px;
}

.iptv__filter-row {
    display: flex;
    gap: 12px;
    @include smartphone-vertical {
        flex-direction: column;
    }
}

.iptv__country-select {
    max-width: 360px;
    @include smartphone-vertical {
        max-width: 100%;
    }
}

.iptv__search {
    max-width: 420px;
    @include smartphone-vertical {
        max-width: 100%;
    }
}

.iptv__group-chips {
    display: flex;
    flex-wrap: wrap;
    gap: 8px;
}

.iptv__chip {
    cursor: pointer;
}

.iptv__chip-count {
    margin-left: 6px;
    font-size: 11px;
    opacity: 0.75;
}

.iptv__alert {
    margin-bottom: 16px;
}

.iptv__alert-detail {
    font-size: 12px;
    word-break: break-all;
}

.iptv__grid {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(280px, 1fr));
    gap: 16px;
    @include smartphone-vertical {
        grid-template-columns: repeat(auto-fill, minmax(150px, 1fr));
        gap: 10px;
    }

    &--empty {
        display: block;
    }
}

.iptv__loading {
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: 16px;
    padding: 80px 0;
}

.iptv__loading-text {
    color: rgb(var(--v-theme-gray));
}

.iptv__empty {
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: 8px;
    padding: 80px 0;
    text-align: center;
    color: rgb(var(--v-theme-gray));

    h2 {
        margin: 0;
        font-size: 18px;
        color: rgb(var(--v-theme-on-background));
    }
}

.iptv__pagination {
    display: flex;
    justify-content: center;
    margin-top: 24px;
}

.iptv-channel-card {
    display: flex;
    align-items: center;
    gap: 12px;
    padding: 12px;
    border-radius: 12px;
    background: rgb(var(--v-theme-background-lighten-1));
    cursor: pointer;
    transition: transform 0.15s ease, background 0.15s ease;

    &:hover {
        transform: translateY(-2px);
        background: rgb(var(--v-theme-background-lighten-2));
    }
}

.iptv-channel-card__logo {
    display: flex;
    flex-shrink: 0;
    align-items: center;
    justify-content: center;
    width: 64px;
    height: 64px;
    padding: 6px;
    border-radius: 10px;
    background: rgb(var(--v-theme-background));
    overflow: hidden;

    img {
        max-width: 100%;
        max-height: 100%;
        object-fit: contain;
    }
}

.iptv-channel-card__info {
    display: flex;
    flex-direction: column;
    gap: 4px;
    min-width: 0;
}

.iptv-channel-card__name {
    font-size: 14px;
    font-weight: 600;
    line-height: 1.3;
    overflow: hidden;
    display: -webkit-box;
    -webkit-line-clamp: 2;
    -webkit-box-orient: vertical;
}

.iptv-channel-card__flag {
    margin-right: 2px;
}

.iptv-channel-card__meta {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 6px;
    font-size: 11px;
    color: rgb(var(--v-theme-gray));
}

.iptv-channel-card__badge {
    padding: 1px 6px;
    border-radius: 999px;
    background: rgb(var(--v-theme-background-lighten-2));
}

.iptv-player__header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
}

.iptv-player__channel-name {
    font-size: 16px;
    font-weight: 600;
}

.iptv-player__channel-group {
    font-size: 12px;
    color: rgb(var(--v-theme-gray));
}

.iptv-player__body {
    padding: 0 0 8px;
}

.iptv-player__video-wrapper {
    position: relative;
    width: 100%;
    aspect-ratio: 16 / 9;
    background: #000;
}

.iptv-player__video {
    width: 100%;
    height: 100%;
    background: #000;
}

.iptv-player__overlay {
    position: absolute;
    inset: 0;
    display: flex;
    flex-direction: column;
    align-items: center;
    justify-content: center;
    gap: 12px;
    color: #fff;
    background: rgba(0, 0, 0, 0.6);

    &--error {
        text-align: center;
        padding: 0 24px;
    }
}

.iptv-player__overlay-text {
    font-size: 13px;
}

.iptv-source__header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
}

.iptv-source__title {
    font-size: 16px;
    font-weight: 600;
}

.iptv-source__section-title {
    margin: 16px 0 8px;
    font-size: 12px;
    font-weight: 600;
    color: rgb(var(--v-theme-gray));
}

.iptv-source__item {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
    padding: 6px 8px;
    border-radius: 8px;
    background: rgb(var(--v-theme-background-lighten-2));

    &--readonly {
        padding: 6px 8px;
    }
}

.iptv-source__item-url {
    font-size: 12px;
    word-break: break-all;
}

.iptv-source__list {
    display: flex;
    flex-direction: column;
    gap: 6px;
}

.iptv-source__empty {
    padding: 8px;
    font-size: 12px;
    color: rgb(var(--v-theme-gray));
}

.iptv-source__add {
    display: flex;
    align-items: center;
    gap: 8px;
}

.iptv-source__add-input {
    flex-grow: 1;
}

</style>
