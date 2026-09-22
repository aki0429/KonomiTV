<template>
    <div class="route-container">
        <HeaderBar />
        <main>
            <Navigation />
            <div class="captures-container-wrapper">
                <SPHeaderBar />
                <div class="captures-container">
                    <Breadcrumbs :crumbs="breadcrumbs" />
                    <!-- ヘッダー: タイトル・件数・ソート -->
                    <div class="captures__header">
                        <h2 class="captures__title">
                            <span class="captures__title-text">{{ pageTitle }}</span>
                            <div class="captures__title-count" v-if="total > 0">{{ total }}件</div>
                            <!-- フォルダ表示時: 名前変更・削除ボタン -->
                            <template v-if="active_folder_id !== null">
                                <v-btn class="captures__title-action ml-2" variant="flat" color="background-lighten-2"
                                    size="small" @click="openRenameFolderDialogForActive()">
                                    <Icon icon="fluent:rename-20-filled" width="18px" />
                                </v-btn>
                                <v-btn class="captures__title-action ml-1" variant="flat" color="background-lighten-2"
                                    size="small" @click="openDeleteFolderDialogForActive()">
                                    <Icon icon="fluent:delete-20-filled" width="18px" />
                                </v-btn>
                            </template>
                        </h2>
                        <div class="captures__actions">
                            <v-select class="captures__sort" color="primary" bg-color="background-lighten-1"
                                variant="solo" density="comfortable" hide-details
                                :items="sort_options" v-model="sort_order"
                                @update:model-value="updateSortOrder">
                            </v-select>
                        </div>
                    </div>
                    <!-- フォルダナビゲーション -->
                    <div class="captures__filters" v-if="!is_search_mode">
                        <!-- フォルダナビゲーションチップ -->
                        <div class="captures__folder-chips">
                            <v-chip class="captures__chip" variant="elevated"
                                :color="active_folder_id === null ? 'primary' : undefined"
                                @click="navigateToFolder(null)">
                                すべて
                            </v-chip>
                            <v-chip class="captures__chip" variant="elevated"
                                v-for="folder in folders" :key="folder.id"
                                :color="active_folder_id === folder.id ? 'primary' : undefined"
                                @click="navigateToFolder(folder.id)"
                                @contextmenu.prevent="openFolderMenu($event, folder)">
                                <Icon icon="fluent:folder-20-filled" width="16px" class="mr-1" />
                                {{ folder.name }}
                                <span class="captures__chip-count">{{ folder.capture_count }}</span>
                            </v-chip>
                            <v-chip class="captures__chip captures__chip--add" variant="tonal"
                                @click="openCreateFolderDialog()">
                                <Icon icon="fluent:add-20-filled" width="16px" class="mr-1" />
                                フォルダ作成
                            </v-chip>
                        </div>
                    </div>
                    <!-- キャプチャグリッド (ローディング中・コンテンツ・空状態をすべて内包) -->
                    <div class="captures__grid"
                        :class="{
                            'captures__grid--loading': is_loading,
                            'captures__grid--empty': total === 0 && !is_loading,
                        }">
                        <!-- 空状態 -->
                        <div class="captures__empty"
                            :class="{'captures__empty--show': total === 0 && !is_loading}">
                            <div class="captures__empty-content">
                                <Icon class="captures__empty-icon" :icon="emptyIcon" width="54px" height="54px" />
                                <h2>{{ emptyMessage }}</h2>
                                <div class="captures__empty-submessage" v-if="emptySubmessage">
                                    {{ emptySubmessage }}
                                </div>
                            </div>
                        </div>
                        <!-- グリッドコンテンツ -->
                        <div class="captures__grid-content">
                            <div class="capture-card" v-for="capture in captures" :key="capture.filename"
                                :class="{'capture-card--selected': selected_filenames.has(capture.filename)}"
                                @click="onCardClick(capture)"
                                @contextmenu.prevent="onCardContextMenu(capture)">
                                <!-- 選択モード時のチェックボックス -->
                                <div class="capture-card__checkbox" v-if="is_selection_mode"
                                    @click.stop="toggleSelection(capture)">
                                    <v-checkbox-btn density="compact" color="primary"
                                        :model-value="selected_filenames.has(capture.filename)"
                                        @update:model-value="toggleSelection(capture)" />
                                </div>
                                <img class="capture-card__image" :src="getCaptureThumbURL(capture)" loading="lazy"
                                    :alt="capture.capture_metadata?.title ?? capture.filename" />
                                <div class="capture-card__overlay">
                                    <span class="capture-card__program-title" v-if="capture.capture_metadata">
                                        {{ capture.capture_metadata.title }}
                                    </span>
                                    <span class="capture-card__date">{{ formatDate(capture.file_modified_at) }}</span>
                                </div>
                            </div>
                        </div>
                    </div>
                    <!-- 選択モードバー -->
                    <v-slide-y-reverse-transition>
                        <div class="captures__selection-bar" v-if="is_selection_mode">
                            <span class="captures__selection-count">{{ selected_filenames.size }}件選択中</span>
                            <v-btn variant="flat" color="primary" size="small" @click="openFolderSelectDialog()">
                                <Icon icon="fluent:folder-add-20-filled" width="18px" class="mr-1" />
                                フォルダに追加
                            </v-btn>
                            <v-btn v-if="active_folder_id !== null" variant="flat" color="warning" size="small"
                                @click="removeSelectedFromFolder()">
                                <Icon icon="fluent:folder-dismiss-20-filled" width="18px" class="mr-1" />
                                フォルダから削除
                            </v-btn>
                            <v-spacer />
                            <v-btn variant="text" size="small" @click="exitSelectionMode()">キャンセル</v-btn>
                        </div>
                    </v-slide-y-reverse-transition>
                    <!-- ページネーション -->
                    <div class="captures__pagination" v-if="total > 0 && !is_loading">
                        <v-pagination v-model="current_page" active-color="primary" density="comfortable"
                            :length="Math.ceil(total / 36)" :total-visible="Utils.isSmartphoneVertical() ? 5 : 7"
                            @update:model-value="updatePage">
                        </v-pagination>
                    </div>
                </div>
            </div>
        </main>
        <!-- ライトボックスダイアログ -->
        <v-dialog content-class="captures-lightbox-dialog" max-width="980" transition="slide-y-transition"
            v-model="lightbox_open">
            <div class="captures-lightbox" v-if="lightbox_capture">
                <img class="captures-lightbox__image"
                    :src="getCaptureFullURL(lightbox_capture)"
                    :alt="lightbox_capture.capture_metadata?.title ?? lightbox_capture.filename" />
                <a v-ripple class="captures-lightbox__download"
                    :href="getCaptureFullURL(lightbox_capture)"
                    :download="lightbox_capture.filename">
                    <Icon icon="fa6-solid:download" width="45px" />
                </a>
                <div class="captures-lightbox__info">
                    <div class="captures-lightbox__metadata">
                        <span class="captures-lightbox__program-title" v-if="lightbox_capture.capture_metadata">
                            {{ lightbox_capture.capture_metadata.title }}
                        </span>
                        <span class="captures-lightbox__filename">{{ lightbox_capture.filename }}</span>
                        <div class="captures-lightbox__details">
                            <span>{{ lightbox_capture.image_width }}x{{ lightbox_capture.image_height }}</span>
                            <span>{{ formatFileSize(lightbox_capture.file_size) }}</span>
                            <span>{{ formatDateTime(lightbox_capture.file_modified_at) }}</span>
                            <span v-if="lightbox_capture.capture_metadata?.is_caption_composited"
                                class="captures-lightbox__badge">字幕あり</span>
                            <span v-if="lightbox_capture.capture_metadata?.is_comment_composited"
                                class="captures-lightbox__badge">コメントあり</span>
                        </div>
                    </div>
                    <div class="captures-lightbox__actions">
                        <v-btn variant="flat" color="primary" size="small"
                            @click="openFolderSelectDialogForSingle(lightbox_capture)">
                            <Icon icon="fluent:folder-add-20-filled" width="18px" class="mr-1" />
                            フォルダに追加
                        </v-btn>
                        <v-btn class="captures-lightbox__delete" variant="flat" color="error" size="small"
                            @click="confirmDelete()">
                            <Icon icon="fluent:delete-20-regular" width="18px" class="mr-1" />
                            削除
                        </v-btn>
                    </div>
                </div>
            </div>
        </v-dialog>
        <!-- 削除確認ダイアログ -->
        <v-dialog v-model="delete_confirm_open" max-width="450" persistent>
            <v-card>
                <v-card-title class="text-h6">キャプチャの削除</v-card-title>
                <v-card-text>
                    このキャプチャを削除しますか？<br>
                    この操作は取り消せません。
                </v-card-text>
                <v-card-actions>
                    <v-spacer />
                    <v-btn variant="text" @click="delete_confirm_open = false">キャンセル</v-btn>
                    <v-btn variant="flat" color="error" :loading="is_deleting" @click="executeDelete()">削除</v-btn>
                </v-card-actions>
            </v-card>
        </v-dialog>
        <!-- フォルダ作成ダイアログ -->
        <v-dialog v-model="create_folder_dialog_open" max-width="450" persistent>
            <v-card>
                <v-card-title class="text-h6">フォルダ作成</v-card-title>
                <v-card-text>
                    <v-text-field v-model="new_folder_name" label="フォルダ名" variant="outlined"
                        density="comfortable" hide-details autofocus
                        @keydown.enter="executeCreateFolder()" />
                </v-card-text>
                <v-card-actions>
                    <v-spacer />
                    <v-btn variant="text" @click="create_folder_dialog_open = false">キャンセル</v-btn>
                    <v-btn variant="flat" color="primary" :loading="is_creating_folder"
                        :disabled="!new_folder_name.trim()" @click="executeCreateFolder()">作成</v-btn>
                </v-card-actions>
            </v-card>
        </v-dialog>
        <!-- フォルダ名前変更ダイアログ -->
        <v-dialog v-model="rename_folder_dialog_open" max-width="450" persistent>
            <v-card>
                <v-card-title class="text-h6">フォルダ名変更</v-card-title>
                <v-card-text>
                    <v-text-field v-model="rename_folder_name" label="フォルダ名" variant="outlined"
                        density="comfortable" hide-details autofocus
                        @keydown.enter="executeRenameFolder()" />
                </v-card-text>
                <v-card-actions>
                    <v-spacer />
                    <v-btn variant="text" @click="rename_folder_dialog_open = false">キャンセル</v-btn>
                    <v-btn variant="flat" color="primary" :loading="is_renaming_folder"
                        :disabled="!rename_folder_name.trim()" @click="executeRenameFolder()">変更</v-btn>
                </v-card-actions>
            </v-card>
        </v-dialog>
        <!-- フォルダ削除確認ダイアログ -->
        <v-dialog v-model="delete_folder_dialog_open" max-width="450" persistent>
            <v-card>
                <v-card-title class="text-h6">フォルダの削除</v-card-title>
                <v-card-text>
                    フォルダ「{{ target_folder?.name }}」を削除しますか？<br>
                    フォルダ内のキャプチャ画像は削除されません。
                </v-card-text>
                <v-card-actions>
                    <v-spacer />
                    <v-btn variant="text" @click="delete_folder_dialog_open = false">キャンセル</v-btn>
                    <v-btn variant="flat" color="error" :loading="is_deleting_folder"
                        @click="executeDeleteFolder()">削除</v-btn>
                </v-card-actions>
            </v-card>
        </v-dialog>
        <!-- フォルダ選択ダイアログ (キャプチャをフォルダに追加する用) -->
        <v-dialog v-model="folder_select_dialog_open" max-width="450">
            <v-card>
                <v-card-title class="text-h6">フォルダに追加</v-card-title>
                <v-card-text class="pb-0">
                    <v-list v-if="folders.length > 0" density="compact">
                        <v-list-item v-for="folder in folders" :key="folder.id"
                            @click="addCapturesToSelectedFolder(folder.id)">
                            <template #prepend>
                                <Icon icon="fluent:folder-20-filled" width="20px" class="mr-2" />
                            </template>
                            <v-list-item-title>{{ folder.name }}</v-list-item-title>
                            <v-list-item-subtitle>{{ folder.capture_count }}件</v-list-item-subtitle>
                        </v-list-item>
                    </v-list>
                    <div v-else class="text-center py-4" style="color: rgb(var(--v-theme-text-darken-1));">
                        フォルダがありません。先にフォルダを作成してください。
                    </div>
                </v-card-text>
                <v-card-actions>
                    <v-spacer />
                    <v-btn variant="text" @click="folder_select_dialog_open = false">閉じる</v-btn>
                </v-card-actions>
            </v-card>
        </v-dialog>
        <!-- フォルダ右クリックメニュー -->
        <v-menu v-model="folder_context_menu_open" :style="{ left: folder_menu_x + 'px', top: folder_menu_y + 'px' }"
            location="start top" absolute>
            <v-list density="compact">
                <v-list-item @click="openRenameFolderDialog()">
                    <template #prepend>
                        <Icon icon="fluent:rename-20-filled" width="20px" class="mr-2" />
                    </template>
                    <v-list-item-title>名前を変更</v-list-item-title>
                </v-list-item>
                <v-list-item @click="openDeleteFolderDialog()">
                    <template #prepend>
                        <Icon icon="fluent:delete-20-filled" width="20px" class="mr-2" />
                    </template>
                    <v-list-item-title>フォルダを削除</v-list-item-title>
                </v-list-item>
            </v-list>
        </v-menu>
    </div>
</template>
<script lang="ts" setup>

import { computed, onMounted, ref, watch } from 'vue';
import { useRoute, useRouter } from 'vue-router';

import Breadcrumbs from '@/components/Breadcrumbs.vue';
import HeaderBar from '@/components/HeaderBar.vue';
import Navigation from '@/components/Navigation.vue';
import SPHeaderBar from '@/components/SPHeaderBar.vue';
import Message from '@/message';
import CapturesService, { ICapture, ICaptureFolder } from '@/services/Captures';
import useUserStore from '@/stores/UserStore';
import Utils, { dayjs } from '@/utils';

// ルーター
const route = useRoute();
const router = useRouter();

// ==================== キャプチャ一覧の状態 ====================

// キャプチャのリスト
const captures = ref<ICapture[]>([]);
const total = ref(0);
const is_loading = ref(true);

// 現在のページ番号
const current_page = ref(1);

// ソート順
const sort_order = ref<'desc' | 'asc'>('desc');
const sort_options = [
    { title: '新しい順', value: 'desc' as const },
    { title: '古い順', value: 'asc' as const },
];

// ==================== 検索の状態 ====================

// 検索モードかどうか (URL パスが /captures/search の場合)
const is_search_mode = computed(() => route.path === '/captures/search');

// 検索クエリ (URL の ?query= から取得)
const search_query = computed(() => {
    return (route.query.query as string) || '';
});

// ==================== フォルダの状態 ====================

// フォルダ一覧
const folders = ref<ICaptureFolder[]>([]);

// 現在表示中のフォルダ ID (null = すべてのキャプチャ)
const active_folder_id = computed<number | null>(() => {
    const folderId = route.params.folder_id as string;
    return folderId ? parseInt(folderId) : null;
});

// フォルダ操作対象
const target_folder = ref<ICaptureFolder | null>(null);

// フォルダ作成ダイアログ
const create_folder_dialog_open = ref(false);
const new_folder_name = ref('');
const is_creating_folder = ref(false);

// フォルダ名前変更ダイアログ
const rename_folder_dialog_open = ref(false);
const rename_folder_name = ref('');
const is_renaming_folder = ref(false);

// フォルダ削除ダイアログ
const delete_folder_dialog_open = ref(false);
const is_deleting_folder = ref(false);

// フォルダ選択ダイアログ (キャプチャ追加用)
const folder_select_dialog_open = ref(false);
// フォルダ追加対象のファイル名リスト (選択モードまたはライトボックスから設定される)
const folder_add_target_filenames = ref<string[]>([]);

// フォルダ右クリックメニュー
const folder_context_menu_open = ref(false);
const folder_menu_x = ref(0);
const folder_menu_y = ref(0);

// ==================== 選択モードの状態 ====================

// 選択モードかどうか
const is_selection_mode = ref(false);

// 選択されたキャプチャのファイル名セット
const selected_filenames = ref<Set<string>>(new Set());

// ==================== ライトボックスの状態 ====================

const lightbox_open = ref(false);
const lightbox_capture = ref<ICapture | null>(null);

// ==================== 削除の状態 ====================

const delete_confirm_open = ref(false);
const is_deleting = ref(false);

// ==================== 算出プロパティ ====================

// パンくずリスト
const breadcrumbs = computed(() => {
    const crumbs = [
        { name: 'ホーム', path: '/' },
        { name: 'キャプチャ', path: '/captures/' },
    ];
    if (is_search_mode.value) {
        crumbs.push({ name: '検索結果', path: '', disabled: true } as any);
    } else if (active_folder_id.value !== null) {
        const folder = folders.value.find(f => f.id === active_folder_id.value);
        crumbs.push({ name: folder?.name ?? 'フォルダ', path: '', disabled: true } as any);
    } else {
        // 最後のアイテムは disabled
        crumbs[crumbs.length - 1] = { ...crumbs[crumbs.length - 1], disabled: true } as any;
    }
    return crumbs;
});

// ページタイトル
const pageTitle = computed(() => {
    if (is_search_mode.value) {
        return `「${search_query.value}」の検索結果`;
    }
    if (active_folder_id.value !== null) {
        const folder = folders.value.find(f => f.id === active_folder_id.value);
        return folder?.name ?? 'フォルダ';
    }
    return 'キャプチャ';
});

// 空状態のアイコン
const emptyIcon = computed(() => {
    if (is_search_mode.value) return 'fluent:search-20-regular';
    if (active_folder_id.value !== null) return 'fluent:folder-open-20-regular';
    return 'fluent:image-multiple-24-regular';
});

// 空状態のメッセージ
const emptyMessage = computed(() => {
    if (is_search_mode.value) return '検索結果が見つかりません。';
    if (active_folder_id.value !== null) return 'このフォルダにはキャプチャがありません。';
    return 'まだキャプチャがありません。';
});

// 空状態のサブメッセージ
const emptySubmessage = computed(() => {
    if (is_search_mode.value) return '別のキーワードで検索してみてください。チャンネル名でも検索できます。';
    if (active_folder_id.value !== null) return 'キャプチャを右クリックまたはロングタップしてフォルダに追加できます。';
    return 'テレビ・ビデオの視聴中にキャプチャすると、ここに表示されます。';
});

// ==================== データ取得 ====================

// キャプチャ一覧を取得する
const fetchCaptures = async () => {
    is_loading.value = true;
    let result;
    if (active_folder_id.value !== null) {
        // フォルダ内キャプチャ一覧を取得
        result = await CapturesService.fetchFolderCaptures(
            active_folder_id.value,
            sort_order.value,
            current_page.value,
        );
    } else {
        // 通常の一覧を取得 (検索クエリはサーバー側で番組名・ファイル名・チャンネル名を横断的に検索する)
        result = await CapturesService.fetchCaptures(
            sort_order.value,
            current_page.value,
            search_query.value || undefined,
        );
    }
    if (result) {
        captures.value = result.captures;
        total.value = result.total;
    }
    is_loading.value = false;
};

// フォルダ一覧を取得する (ログインしていない場合やサーバーが未対応の場合はサイレントに空リストのまま)
const fetchFolders = async () => {
    const userStore = useUserStore();
    // ログインしていない場合はフォルダ機能を使えないため、取得をスキップする
    if (userStore.is_logged_in === false) return;
    const result = await CapturesService.fetchFolders();
    if (result) {
        folders.value = result.folders;
    }
};

// ==================== URL / ナビゲーション ====================

// サムネイル画像の URL を取得する
const getCaptureThumbURL = (capture: ICapture): string => {
    return CapturesService.getCaptureImageURL(capture.filename, true);
};

// フルサイズ画像の URL を取得する
const getCaptureFullURL = (capture: ICapture): string => {
    return CapturesService.getCaptureImageURL(capture.filename, false);
};

// フォルダに移動する
const navigateToFolder = (folderId: number | null) => {
    exitSelectionMode();
    if (folderId === null) {
        router.push('/captures/');
    } else {
        router.push(`/captures/folders/${folderId}`);
    }
};

// ==================== カードクリック / 選択モード ====================

// カードクリック時の処理 (選択モードかどうかで分岐)
const onCardClick = (capture: ICapture) => {
    if (is_selection_mode.value) {
        toggleSelection(capture);
    } else {
        openLightbox(capture);
    }
};

// カード右クリック → 選択モードに入る
const onCardContextMenu = (capture: ICapture) => {
    if (!is_selection_mode.value) {
        is_selection_mode.value = true;
        selected_filenames.value = new Set();
    }
    // トグル選択
    toggleSelection(capture);
};

// 選択のトグル
const toggleSelection = (capture: ICapture) => {
    const newSet = new Set(selected_filenames.value);
    if (newSet.has(capture.filename)) {
        newSet.delete(capture.filename);
    } else {
        newSet.add(capture.filename);
    }
    selected_filenames.value = newSet;
    // 選択が0件になったら選択モードを終了
    if (newSet.size === 0) {
        is_selection_mode.value = false;
    }
};

// 選択モードを終了する
const exitSelectionMode = () => {
    is_selection_mode.value = false;
    selected_filenames.value = new Set();
};

// ==================== ライトボックス ====================

// ライトボックスを開く
const openLightbox = (capture: ICapture) => {
    lightbox_capture.value = capture;
    lightbox_open.value = true;
};

// ==================== 削除 ====================

// 削除確認ダイアログを表示する
const confirmDelete = () => {
    delete_confirm_open.value = true;
};

// 削除を実行する
const executeDelete = async () => {
    if (!lightbox_capture.value) return;
    is_deleting.value = true;
    const success = await CapturesService.deleteCapture(lightbox_capture.value.filename);
    is_deleting.value = false;
    if (success) {
        Message.success('キャプチャを削除しました。');
        delete_confirm_open.value = false;
        lightbox_open.value = false;
        lightbox_capture.value = null;
        // 一覧を再取得する
        await fetchCaptures();
    }
};

// ==================== フォルダ操作 ====================

// フォルダ作成ダイアログを開く
const openCreateFolderDialog = () => {
    new_folder_name.value = '';
    create_folder_dialog_open.value = true;
};

// フォルダ作成を実行する
const executeCreateFolder = async () => {
    if (!new_folder_name.value.trim()) return;
    is_creating_folder.value = true;
    const result = await CapturesService.createFolder(new_folder_name.value.trim());
    is_creating_folder.value = false;
    if (result) {
        Message.success(`フォルダ「${result.name}」を作成しました。`);
        create_folder_dialog_open.value = false;
        await fetchFolders();
    }
};

// フォルダ右クリックメニューを開く
const openFolderMenu = (event: MouseEvent, folder: ICaptureFolder) => {
    target_folder.value = folder;
    folder_menu_x.value = event.clientX;
    folder_menu_y.value = event.clientY;
    folder_context_menu_open.value = true;
};

// フォルダ名前変更ダイアログを開く
const openRenameFolderDialog = () => {
    if (!target_folder.value) return;
    rename_folder_name.value = target_folder.value.name;
    rename_folder_dialog_open.value = true;
};

// フォルダ名前変更を実行する
const executeRenameFolder = async () => {
    if (!target_folder.value || !rename_folder_name.value.trim()) return;
    is_renaming_folder.value = true;
    const result = await CapturesService.updateFolder(target_folder.value.id, {
        name: rename_folder_name.value.trim(),
    });
    is_renaming_folder.value = false;
    if (result) {
        Message.success(`フォルダ名を「${result.name}」に変更しました。`);
        rename_folder_dialog_open.value = false;
        await fetchFolders();
    }
};

// フォルダ削除ダイアログを開く
const openDeleteFolderDialog = () => {
    if (!target_folder.value) return;
    delete_folder_dialog_open.value = true;
};

// ヘッダーボタンから名前変更ダイアログを開く (アクティブなフォルダを対象にする)
const openRenameFolderDialogForActive = () => {
    const folder = folders.value.find(f => f.id === active_folder_id.value);
    if (!folder) return;
    target_folder.value = folder;
    openRenameFolderDialog();
};

// ヘッダーボタンから削除ダイアログを開く (アクティブなフォルダを対象にする)
const openDeleteFolderDialogForActive = () => {
    const folder = folders.value.find(f => f.id === active_folder_id.value);
    if (!folder) return;
    target_folder.value = folder;
    openDeleteFolderDialog();
};

// フォルダ削除を実行する
const executeDeleteFolder = async () => {
    if (!target_folder.value) return;
    is_deleting_folder.value = true;
    const success = await CapturesService.deleteFolder(target_folder.value.id);
    is_deleting_folder.value = false;
    if (success) {
        Message.success(`フォルダ「${target_folder.value.name}」を削除しました。`);
        delete_folder_dialog_open.value = false;
        // 削除したフォルダを表示中だった場合は全体一覧に戻る
        if (active_folder_id.value === target_folder.value.id) {
            router.push('/captures/');
        }
        await fetchFolders();
    }
};

// フォルダ選択ダイアログを開く (複数選択モードから)
const openFolderSelectDialog = () => {
    folder_add_target_filenames.value = Array.from(selected_filenames.value);
    folder_select_dialog_open.value = true;
};

// フォルダ選択ダイアログを開く (ライトボックスから単一キャプチャ)
const openFolderSelectDialogForSingle = (capture: ICapture) => {
    folder_add_target_filenames.value = [capture.filename];
    folder_select_dialog_open.value = true;
};

// 選択されたフォルダにキャプチャを追加する
const addCapturesToSelectedFolder = async (folderId: number) => {
    const filenames = folder_add_target_filenames.value;
    if (filenames.length === 0) return;

    const success = await CapturesService.addCapturesToFolder(folderId, filenames);
    if (success) {
        const folder = folders.value.find(f => f.id === folderId);
        Message.success(`${filenames.length}件のキャプチャを「${folder?.name ?? 'フォルダ'}」に追加しました。`);
        folder_select_dialog_open.value = false;
        exitSelectionMode();
        // フォルダ一覧を更新 (capture_count が変わるため)
        await fetchFolders();
    }
};

// 選択中のキャプチャを現在のフォルダから削除する
const removeSelectedFromFolder = async () => {
    if (active_folder_id.value === null) return;
    const filenames = Array.from(selected_filenames.value);
    if (filenames.length === 0) return;

    const success = await CapturesService.removeCapturesFromFolder(active_folder_id.value, filenames);
    if (success) {
        Message.success(`${filenames.length}件のキャプチャをフォルダから削除しました。`);
        exitSelectionMode();
        await fetchCaptures();
        await fetchFolders();
    }
};

// ==================== フォーマッタ ====================

// 日付をフォーマットする (グリッドカード用)
const formatDate = (dateStr: string): string => {
    return dayjs(dateStr).format('YYYY/MM/DD HH:mm');
};

// 日時をフォーマットする (ライトボックス用)
const formatDateTime = (dateStr: string): string => {
    return dayjs(dateStr).format('YYYY/MM/DD HH:mm:ss');
};

// ファイルサイズをフォーマットする
const formatFileSize = (bytes: number): string => {
    if (bytes < 1024) return `${bytes} B`;
    if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
    return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
};

// ==================== ページ / ソート更新 ====================

// ページを更新する
const updatePage = async (page: number) => {
    current_page.value = page;
    is_loading.value = true;
    await router.replace({
        query: {
            ...route.query,
            page: page.toString(),
        },
    });
};

// ソート順を更新する
const updateSortOrder = async () => {
    current_page.value = 1;
    is_loading.value = true;
    await router.replace({
        query: {
            ...route.query,
            order: sort_order.value,
            page: '1',
        },
    });
};

// ==================== ルート変更の監視 ====================

// クエリパラメータが変更されたらキャプチャを再取得する
watch(() => route.fullPath, async () => {
    // ページ番号を同期
    if (route.query.page) {
        current_page.value = parseInt(route.query.page as string);
    } else {
        current_page.value = 1;
    }
    // ソート順を同期
    if (route.query.order) {
        sort_order.value = route.query.order as 'desc' | 'asc';
    }
    await fetchCaptures();
}, { deep: true });

// ==================== 初期化 ====================

onMounted(async () => {
    // 事前にログイン状態を同期（トークンがあればユーザー情報を取得）
    const userStore = useUserStore();
    await userStore.fetchUser();

    // クエリパラメータから初期値を設定
    if (route.query.page) {
        current_page.value = parseInt(route.query.page as string);
    }
    if (route.query.order) {
        sort_order.value = route.query.order as 'desc' | 'asc';
    }

    // フォルダ一覧を取得
    await fetchFolders();

    // キャプチャ一覧を取得
    await fetchCaptures();
});

</script>
<style lang="scss" scoped>

.captures-container-wrapper {
    display: flex;
    flex-direction: column;
    width: 100%;
    @include smartphone-vertical {
        padding-top: 10px !important;
    }
}

.captures-container {
    display: flex;
    flex-direction: column;
    width: 100%;
    height: 100%;
    padding: 20px;
    margin: 0 auto;
    min-width: 0;
    max-width: 1000px;
    @include smartphone-horizontal {
        padding: 16px 20px !important;
    }
    @include smartphone-horizontal-short {
        padding: 16px 16px !important;
    }
    @include smartphone-vertical {
        padding: 16px 8px !important;
        padding-top: 8px !important;
    }
}

// ヘッダー: タイトル・件数・ソート (RecordedProgramList のヘッダーパターンに準拠)
.captures__header {
    display: flex;
    align-items: center;
    @include smartphone-vertical {
        padding: 0px 8px;
    }
}

.captures__title {
    display: flex;
    align-items: center;
    position: relative;
    font-size: 24px;
    font-weight: 700;
    padding-top: 8px;
    padding-bottom: 20px;
    @include smartphone-vertical {
        font-size: 22px;
        padding-bottom: 16px;
    }

    &-count {
        display: flex;
        align-items: center;
        flex-shrink: 0;
        padding-top: 8px;
        margin-left: 12px;
        font-size: 14px;
        font-weight: 400;
        color: rgb(var(--v-theme-text-darken-1));
    }

    &-action {
        min-width: 36px !important;
        width: 36px;
        height: 36px;
        padding: 0 !important;
        border-radius: 8px;
    }
}

.captures__actions {
    display: flex;
    align-items: center;
    margin-left: auto;
    :deep(.v-field) {
        padding-right: 4px !important;
    }
    :deep(.v-field__input) {
        padding-left: 12px !important;
        padding-right: 0px !important;
    }
}

.captures__sort {
    width: 103px;
    :deep(.v-field__input) {
        font-size: 14px !important;
        padding-top: 6px !important;
        padding-bottom: 6px !important;
        min-height: unset !important;
    }
}

// チャンネルフィルタ & フォルダナビゲーション
.captures__filters {
    display: flex;
    flex-direction: column;
    gap: 8px;
    margin-bottom: 12px;
    @include smartphone-vertical {
        padding: 0px 8px;
        margin-bottom: 8px;
    }
}

.captures__folder-chips {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
}

.captures__chip {
    cursor: pointer;
    font-size: 13px;
    &--add {
        opacity: 0.8;
    }
}

.captures__chip-count {
    margin-left: 4px;
    font-size: 11px;
    opacity: 0.7;
}

// キャプチャグリッド
// display: block を使うことで、子要素の CSS Grid レイアウトが確実に親の全幅で展開されるようにする
// (空状態の .captures__empty は position: absolute なので block レイアウトでも問題なし)
.captures__grid {
    position: relative;
    width: 100%;
    background: rgb(var(--v-theme-background-lighten-1));
    border-radius: 8px;
    overflow: hidden;

    &--loading {
        .captures__grid-content {
            visibility: hidden;
            opacity: 0;
        }
    }
    &--empty {
        min-height: 200px;
    }
}

.captures__grid-content {
    display: grid;
    // デフォルトは4列 (デスクトップ・タブレット横・タブレット縦すべて)
    // smartphone-vertical のみ2列に切り替える
    grid-template-columns: repeat(4, 1fr);
    gap: 4px;
    padding: 4px;
    width: 100%;
    transition: visibility 0.2s ease, opacity 0.2s ease;
    @include smartphone-horizontal {
        grid-template-columns: repeat(3, 1fr);
    }
    @include smartphone-horizontal-short {
        grid-template-columns: repeat(3, 1fr);
    }
    @include smartphone-vertical {
        grid-template-columns: repeat(2, 1fr);
    }
}

// キャプチャカード
.capture-card {
    position: relative;
    aspect-ratio: 16 / 9;
    border-radius: 7px;
    overflow: hidden;
    cursor: pointer;
    // グリッドアイテムがグリッドセルの幅を超えないようにする
    min-width: 0;
    // 読み込まれるまでのキャプチャの背景 (既存のキャプチャコンポーネントと同じグラデーション)
    background: linear-gradient(150deg, rgb(var(--v-theme-gray)), rgb(var(--v-theme-background-lighten-2)));
    // content-visibility: auto は最終行のグリッドレイアウトを壊すことがあるため使用しない
    transition: opacity 0.15s ease;

    &:hover {
        opacity: 0.85;
    }

    &--selected {
        outline: 3px solid rgb(var(--v-theme-primary));
        outline-offset: -3px;
    }

    &__checkbox {
        position: absolute;
        top: 4px;
        left: 4px;
        z-index: 2;
        background: rgba(0, 0, 0, 0.5);
        border-radius: 4px;
    }

    &__image {
        display: block;
        width: 100%;
        height: 100%;
        object-fit: cover;
    }

    &__overlay {
        position: absolute;
        bottom: 0;
        left: 0;
        right: 0;
        padding: 24px 8px 6px;
        background: linear-gradient(transparent, rgba(0, 0, 0, 0.75));
        display: flex;
        flex-direction: column;
        gap: 1px;
    }

    &__program-title {
        font-size: 12px;
        font-weight: bold;
        color: #ffffff;
        white-space: nowrap;
        overflow: hidden;
        text-overflow: ellipsis;
        @include smartphone-vertical {
            font-size: 11px;
        }
    }

    &__date {
        font-size: 11px;
        color: rgba(255, 255, 255, 0.8);
        @include smartphone-vertical {
            font-size: 10px;
        }
    }
}

// 空状態 (RecordedProgramList の __empty パターンに準拠)
.captures__empty {
    position: absolute;
    top: 0;
    left: 0;
    width: 100%;
    height: 100%;
    display: flex;
    justify-content: center;
    align-items: center;
    padding-top: 28px;
    padding-bottom: 40px;
    flex-grow: 1;
    visibility: hidden;
    opacity: 0;
    transition: visibility 0.2s ease, opacity 0.2s ease;

    &--show {
        visibility: visible;
        opacity: 1;
    }

    &-content {
        text-align: center;

        .captures__empty-icon {
            color: rgb(var(--v-theme-text-darken-1));
        }

        h2 {
            font-size: 21px;
            @include tablet-vertical {
                font-size: 19px !important;
            }
            @include smartphone-horizontal {
                font-size: 19px !important;
            }
            @include smartphone-horizontal-short {
                font-size: 19px !important;
            }
            @include smartphone-vertical {
                font-size: 19px !important;
                text-align: center;
            }
        }

        .captures__empty-submessage {
            margin-top: 8px;
            color: rgb(var(--v-theme-text-darken-1));
            font-size: 15px;
            @include tablet-vertical {
                font-size: 13px !important;
                text-align: center;
            }
            @include smartphone-horizontal {
                font-size: 13px !important;
                text-align: center;
            }
            @include smartphone-vertical {
                font-size: 13px !important;
                text-align: center;
                margin-top: 7px !important;
                line-height: 1.65;
            }
        }
    }
}

// 選択モードバー
.captures__selection-bar {
    position: fixed;
    bottom: 16px;
    left: 50%;
    transform: translateX(-50%);
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 8px 16px;
    background: rgb(var(--v-theme-background-lighten-1));
    border-radius: 12px;
    box-shadow: 0px 4px 12px rgba(0, 0, 0, 0.3);
    z-index: 100;
    @include smartphone-vertical {
        bottom: 70px;
        left: 8px;
        right: 8px;
        transform: none;
    }
}

.captures__selection-count {
    font-size: 14px;
    font-weight: 600;
    white-space: nowrap;
    margin-right: 4px;
}

// ページネーション
.captures__pagination {
    display: flex;
    justify-content: center;
    margin-top: 24px;
    @include smartphone-vertical {
        margin-top: 20px;
    }
}

</style>
<style lang="scss">

// ライトボックスダイアログ (v-dialog の content-class でスコープ外に適用する必要がある)
// 既存の zoom-capture-modal パターンに準拠
@include smartphone-horizontal {
    .captures-lightbox-dialog {
        width: auto !important;
        max-width: auto !important;
    }
}

.captures-lightbox-dialog {
    margin: 12px;

    .captures-lightbox {
        position: relative;
        display: flex;
        flex-direction: column;
        background: rgb(var(--v-theme-background-lighten-1));
        border-radius: 11px;
        overflow: hidden;

        &__image {
            display: block;
            width: 100%;
            max-height: 70vh;
            object-fit: contain;
            background: #000000;
        }

        // ダウンロードボタン (zoom-capture-modal__download パターンに準拠)
        &__download {
            display: flex;
            position: absolute;
            align-items: center;
            justify-content: center;
            right: 22px;
            bottom: calc(84px + 20px);  // info 領域の高さ + bottom 余白
            width: 80px;
            height: 80px;
            border-radius: 50%;
            color: rgb(var(--v-theme-text));
            filter: drop-shadow(0px 0px 4.5px rgba(0, 0, 0, 90%));
            z-index: 1;
            @include smartphone-vertical {
                right: 14px;
                bottom: calc(120px + 14px);  // スマホでは info 領域が縦になるため位置調整
                width: 60px;
                height: 60px;
            }
        }

        &__info {
            display: flex;
            align-items: center;
            justify-content: space-between;
            padding: 14px 16px;
            gap: 12px;
            @include smartphone-vertical {
                flex-direction: column;
                align-items: flex-start;
            }
        }

        &__metadata {
            display: flex;
            flex-direction: column;
            gap: 2px;
            min-width: 0;
            flex: 1;
        }

        &__program-title {
            font-size: 15px;
            font-weight: bold;
            color: rgb(var(--v-theme-text));
            white-space: nowrap;
            overflow: hidden;
            text-overflow: ellipsis;
        }

        &__filename {
            font-size: 13px;
            color: rgb(var(--v-theme-text-darken-1));
            white-space: nowrap;
            overflow: hidden;
            text-overflow: ellipsis;
        }

        &__details {
            display: flex;
            flex-wrap: wrap;
            gap: 8px;
            font-size: 12px;
            color: rgb(var(--v-theme-text-darken-1));
        }

        &__badge {
            padding: 1px 6px;
            border-radius: 4px;
            background: rgb(var(--v-theme-primary));
            color: #ffffff;
            font-size: 11px;
        }

        &__actions {
            display: flex;
            gap: 8px;
            flex-shrink: 0;
        }

        &__delete {
            flex-shrink: 0;
        }
    }
}

</style>
