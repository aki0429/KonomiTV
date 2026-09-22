
import APIClient from '@/services/APIClient';
import Utils from '@/utils';


/** IPTV ストリームの配信フォーマット (サーバー側の DetectStreamType() に対応) */
export type IPTVStreamType = 'HLS' | 'DASH' | 'MP4' | 'MPEGTS' | 'Other';

/** IPTV チャンネルの情報を表すインターフェイス (サーバー側の IPTVChannel に対応) */
export interface IIPTVChannel {
    id: string;
    name: string;
    logo_url: string | null;
    group: string | null;
    country: string | null;
    country_name: string | null;
    country_flag: string | null;
    tvg_id: string | null;
    language: string | null;
    is_geo_blocked: boolean;
    source_url: string;
    stream_url: string;
    // テレビ視聴 UI (/tv/watch/) で再生するための疑似チャンネル ID
    display_channel_id: string;
    stream_type: IPTVStreamType;
    is_hls: boolean;
    // テレビ視聴 UI に登録済みかどうか
    is_tvui_registered: boolean;
}

/** IPTV チャンネル一覧レスポンスを表すインターフェイス (サーバー側の IPTVChannels に対応) */
export interface IIPTVChannels {
    total: number;
    page: number;
    per_page: number;
    max_page: number;
    all_total: number;
    updated_at: number | null;
    sources: string[];
    errors: Record<string, string>;
    channels: IIPTVChannel[];
}

/** IPTV の国情報を表すインターフェイス (サーバー側の IPTVCountry に対応) */
export interface IIPTVCountry {
    code: string;
    name: string;
    flag: string;
    count: number;
}

/** IPTV 国一覧レスポンスを表すインターフェイス (サーバー側の IPTVCountries に対応) */
export interface IIPTVCountries {
    total: number;
    updated_at: number | null;
    all_total: number;
    countries: IIPTVCountry[];
}

/** IPTV のグループ情報を表すインターフェイス (サーバー側の IPTVGroup に対応) */
export interface IIPTVGroup {
    name: string;
    count: number;
}

/** IPTV グループ一覧レスポンスを表すインターフェイス (サーバー側の IPTVGroups に対応) */
export interface IIPTVGroups {
    total: number;
    groups: IIPTVGroup[];
}

/** IPTV プレイリストソース一覧レスポンスを表すインターフェイス (サーバー側の IPTVSources に対応) */
export interface IIPTVSources {
    config_sources: string[];
    user_sources: string[];
}

/** テレビ視聴 UI に登録された IPTV チャンネルを表すインターフェイス (サーバー側の IPTVTVUIChannel に対応) */
export interface IIPTVTVUIChannel {
    display_channel_id: string;
    name: string;
    logo_url: string | null;
    country_name: string | null;
}

/** テレビ視聴 UI に登録された IPTV チャンネル一覧レスポンスを表すインターフェイス (サーバー側の IPTVTVUIChannels に対応) */
export interface IIPTVTVUIChannels {
    total: number;
    channels: IIPTVTVUIChannel[];
}

/** IPTV チャンネル一覧取得時のクエリパラメーター */
export interface IIPTVChannelQuery {
    country?: string | null;
    group?: string | null;
    search?: string | null;
    page?: number;
    per_page?: number;
    refresh?: boolean;
}


class IPTV {

    /**
     * IPTV のチャンネル一覧を取得する
     * @param query 絞り込み条件・ページネーションの指定
     * @param suppress_error エラーメッセージを表示しない場合は true
     * @returns チャンネル一覧 or 取得に失敗した場合は null
     */
    static async fetchChannels(query: IIPTVChannelQuery = {}, suppress_error: boolean = false): Promise<IIPTVChannels | null> {

        // 未指定のパラメーターは送信しない
        const params: Record<string, string | number | boolean> = {
            page: query.page ?? 1,
            per_page: query.per_page ?? 60,
            refresh: query.refresh ?? false,
        };
        if (query.country !== undefined && query.country !== null && query.country !== '') {
            params.country = query.country;
        }
        if (query.group !== undefined && query.group !== null && query.group !== '') {
            params.group = query.group;
        }
        if (query.search !== undefined && query.search !== null && query.search !== '') {
            params.search = query.search;
        }

        // API リクエストを実行
        const response = await APIClient.get<IIPTVChannels>('/iptv/channels', {
            params: params,
            // プレイリストの取得に時間がかかることがあるため、タイムアウトを 90 秒に伸ばす
            timeout: 90 * 1000,
        });

        // エラー処理
        if (response.type === 'error') {
            if (suppress_error === false) {
                APIClient.showGenericError(response, 'IPTV のチャンネル一覧を取得できませんでした。');
            }
            return null;
        }

        return response.data;
    }

    /**
     * 取り込んだ IPTV チャンネルに含まれる国の一覧を取得する
     * @param suppress_error エラーメッセージを表示しない場合は true
     * @returns 国の一覧 or 取得に失敗した場合は null
     */
    static async fetchCountries(suppress_error: boolean = false): Promise<IIPTVCountries | null> {

        const response = await APIClient.get<IIPTVCountries>('/iptv/countries', {
            timeout: 90 * 1000,
        });

        if (response.type === 'error') {
            if (suppress_error === false) {
                APIClient.showGenericError(response, 'IPTV の国一覧を取得できませんでした。');
            }
            return null;
        }

        return response.data;
    }

    /**
     * 指定された国に含まれるグループの一覧を取得する
     * @param country 国コード (null の場合はすべての国)
     * @param suppress_error エラーメッセージを表示しない場合は true
     * @returns グループの一覧 or 取得に失敗した場合は null
     */
    static async fetchGroups(country: string | null = null, suppress_error: boolean = false): Promise<IIPTVGroups | null> {

        const params: Record<string, string> = {};
        if (country !== null && country !== '') {
            params.country = country;
        }

        const response = await APIClient.get<IIPTVGroups>('/iptv/groups', {
            params: params,
            timeout: 90 * 1000,
        });

        if (response.type === 'error') {
            if (suppress_error === false) {
                APIClient.showGenericError(response, 'IPTV のグループ一覧を取得できませんでした。');
            }
            return null;
        }

        return response.data;
    }

    /**
     * 登録されている IPTV プレイリストのソース一覧を取得する
     * @param suppress_error エラーメッセージを表示しない場合は true
     * @returns ソース一覧 or 取得に失敗した場合は null
     */
    static async fetchSources(suppress_error: boolean = false): Promise<IIPTVSources | null> {

        const response = await APIClient.get<IIPTVSources>('/iptv/sources');

        if (response.type === 'error') {
            if (suppress_error === false) {
                APIClient.showGenericError(response, 'IPTV のソース一覧を取得できませんでした。');
            }
            return null;
        }

        return response.data;
    }

    /**
     * IPTV プレイリストのソースを追加登録する
     * @param url M3U プレイリストの URL
     * @returns 更新後のソース一覧 or 追加に失敗した場合は null
     */
    static async addSource(url: string): Promise<IIPTVSources | null> {

        const response = await APIClient.post<IIPTVSources>('/iptv/sources', {url: url}, {
            timeout: 90 * 1000,
        });

        if (response.type === 'error') {
            APIClient.showGenericError(response, 'IPTV のソースを追加できませんでした。');
            return null;
        }

        return response.data;
    }

    /**
     * 追加登録した IPTV プレイリストのソースを削除する
     * @param url 削除する M3U プレイリストの URL
     * @returns 更新後のソース一覧 or 削除に失敗した場合は null
     */
    static async removeSource(url: string): Promise<IIPTVSources | null> {

        const response = await APIClient.delete<IIPTVSources>('/iptv/sources', {
            params: {url: url},
            timeout: 90 * 1000,
        });

        if (response.type === 'error') {
            APIClient.showGenericError(response, 'IPTV のソースを削除できませんでした。');
            return null;
        }

        return response.data;
    }

    /**
     * IPTV チャンネルをテレビ視聴 UI (/tv/watch/) で再生できるように登録する
     * @param display_channel_id 登録する IPTV チャンネルの display_channel_id
     * @returns 登録に成功したかどうか
     */
    static async registerForTV(display_channel_id: string): Promise<boolean> {

        const response = await APIClient.post<IIPTVTVUIChannels>('/iptv/tvui', {display_channel_id: display_channel_id}, {
            // プレイリストが未取得の場合はサーバー側で取得されるため、タイムアウトを 90 秒に伸ばす
            timeout: 90 * 1000,
        });

        if (response.type === 'error') {
            APIClient.showGenericError(response, 'IPTV チャンネルをテレビ視聴 UI に登録できませんでした。');
            return false;
        }

        return true;
    }

    /**
     * チャンネルロゴの URL を、KonomiTV サーバーのロゴプロキシ経由の URL に変換する
     * @param logo_url 元のロゴの URL
     * @returns ロゴプロキシ経由の URL
     */
    static getLogoURL(logo_url: string | null): string | null {
        if (logo_url === null) {
            return null;
        }
        return `${Utils.api_base_url}/iptv/logo?url=${encodeURIComponent(logo_url)}`;
    }

    /**
     * ストリームの URL (サーバーのプロキシ API のパス) を絶対 URL に変換する
     * @param stream_url サーバーから返されたプロキシ API のパス
     * @returns 絶対 URL
     */
    static getStreamURL(stream_url: string): string {
        // 既に絶対 URL の場合はそのまま返す
        if (stream_url.startsWith('http://') || stream_url.startsWith('https://')) {
            return stream_url;
        }
        // Utils.api_base_url は /api で終わるため、/iptv/... の前の /api を除去して結合する
        return `${Utils.api_base_url}${stream_url.replace(/^\/api/, '')}`;
    }
}

export default IPTV;
