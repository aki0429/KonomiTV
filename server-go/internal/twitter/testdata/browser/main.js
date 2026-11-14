// Twitter には接続せず、ローカル HTTP サーバーへの送信だけを発生させる検証用 Web App。
const operations = ['Viewer', 'CreateTweet', 'CreateRetweet', 'DeleteRetweet', 'FavoriteTweet', 'UnfavoriteTweet', 'HomeLatestTimeline', 'SearchTimeline'];
const modules = {};
operations.forEach((operation, index) => { modules['op'+index] = module => {module.exports = {operationName:operation, queryId:'local'};}; });
modules.client = module => {
    module.exports = class FixtureClient {
        constructor() {
            this.graphQL = async (operation, variables, _, flags) => new Promise((resolve, reject) => {
                const xhr = new XMLHttpRequest();
                xhr.open('POST', location.origin+'/graphql/'+operation.operationName+'?mode='+encodeURIComponent(window.fixtureMode || ''));
                xhr.setRequestHeader('Content-Type', 'application/json');
                xhr.onload = () => resolve(xhr.responseText);
                xhr.onerror = () => reject(new Error('local request failed'));
                xhr.send(JSON.stringify({variables, flags, reply:history.state?.state?.inReplyToStatusId}));
            });
            this.graphQLFullResponse = this.graphQL;
        }
        dispatch() {}
    };
};
self.webpackChunk_twitter_responsive_web.push([[1], modules, require => {
    operations.forEach((_, index) => require('op'+index));
    const Client = require('client');
    window.fixtureClient = new Client();
}]);

window.fixtureMode = '';
window.fixtureImages = [];
function openFixtureModal() {
    if (document.querySelector('[role="dialog"]')) return;
    history.replaceState(history.state, '', '/compose/post');
    const modal = document.createElement('div');
    modal.setAttribute('role', 'dialog');
    modal.innerHTML = '<div contenteditable="true" tabindex="0" data-testid="tweetTextarea_0"></div><input type="file" multiple data-testid="fileInput"><button data-testid="tweetButton" aria-disabled="false">ローカルへ送信</button>';
    document.body.append(modal);
    const textarea = modal.querySelector('[data-testid="tweetTextarea_0"]');
    textarea.addEventListener('paste', event => {
        event.preventDefault();
        textarea.innerText = event.clipboardData.getData('text/plain');
    });
    modal.querySelector('input').addEventListener('change', async event => {
        window.fixtureImages = [];
        for (const file of event.target.files) {
            const bytes = new Uint8Array(await file.arrayBuffer());
            window.fixtureImages.push({name:file.name, type:file.type, base64:btoa(String.fromCharCode(...bytes))});
            await fetch('/upload.x.com/FINALIZE?mode='+encodeURIComponent(window.fixtureMode), {method:'POST', body:JSON.stringify(window.fixtureImages.at(-1))});
        }
    });
    textarea.focus();
}
window.addEventListener('popstate', openFixtureModal);
document.addEventListener('keydown', event => {
    if (event.key === 'n' && !document.querySelector('[role="dialog"]')) openFixtureModal();
    if (event.key === 'Escape') {
        document.querySelector('[role="dialog"]')?.remove();
        history.replaceState(null, '', '/');
        window.fixtureImages = [];
        document.body.focus();
    }
    if (event.ctrlKey && event.key === 'Enter') {
        event.preventDefault();
        const textarea = document.querySelector('[role="dialog"] [data-testid="tweetTextarea_0"]');
        window.fixtureClient.graphQL({operationName:'CreateTweet'}, {text:textarea.innerText, images:window.fixtureImages}).then(() => {
            document.querySelector('[role="dialog"]')?.remove();
            history.replaceState(null, '', '/');
            window.fixtureImages = [];
            document.body.focus();
        });
    }
});
