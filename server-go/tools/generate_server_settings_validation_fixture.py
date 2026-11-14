"""固定した本家のサーバー設定モデルを環境の検査を含めて実行する。"""
import ast
import asyncio
import concurrent.futures
import __future__
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import subprocess
import sys
import tempfile
from types import ModuleType, SimpleNamespace
from typing import Annotated, Any, Literal, cast
from importlib.metadata import version

import httpx
from pydantic import BaseModel, DirectoryPath, FilePath, Field, PositiveFloat, PositiveInt, UrlConstraints, ValidationError, ValidationInfo, confloat, field_validator
from pydantic_core import Url
from fastapi import FastAPI, Body, Depends, HTTPException, Request, status
from fastapi.responses import Response
from fastapi.testclient import TestClient

ROOT = Path(__file__).resolve().parents[2]
REF = '687b904bb0a2cbb329596fd7b0368c09afe19a6c'
EXPECTED = {'fastapi':'0.162.0','pydantic':'2.14.3','starlette':'1.10.0','tortoise-orm':'1.1.9','httpx':'0.28.1'}
RUNTIME = {name:version(name) for name in EXPECTED}
RUNTIME['python'] = sys.version.split()[0]
assert all(RUNTIME[name] == value for name,value in EXPECTED.items()) and sys.version_info[:2] == (3,13)
def source(path):
    return subprocess.check_output(['git','-C',str(ROOT),'show',f'{REF}:{path}']).decode('utf-8')

config_source = source('server/app/config.py')
region = next(node for node in ast.parse(source('server/app/utils/TSInformation.py')).body if isinstance(node, ast.Assign) and isinstance(node.targets[0],ast.Name) and node.targets[0].id == 'TerrestrialRegion')
env = globals()
exec(compile(ast.Module(body=[region],type_ignores=[]),'upstream/TSInformation.py','exec'),env)
classes = [node for node in ast.parse(config_source).body if isinstance(node,ast.ClassDef) and (node.name == 'ServerSettings' or node.name.startswith('_ServerSettings'))]
API_REQUEST_HEADERS = {'User-Agent':'KonomiTV/test'}
LIBRARY_PATH = {name:name for name in ['FFmpeg','QSVEncC','NVEncC','VCEEncC','rkmppenc']}
exec(compile(ast.Module(body=classes,type_ignores=[]),'upstream/config.py','exec',flags=__future__.annotations.compiler_flag),env)

scenario = {}
calls = []
logging = SimpleNamespace(info=lambda *args:None,warning=lambda *args:None,error=lambda *args:None)
app_module = ModuleType('app'); app_module.logging = logging
sys.modules['app'] = app_module
utils_module = ModuleType('app.utils')
sys.modules['app.utils'] = utils_module
utils_source = source('server/app/utils/__init__.py')
folder_function = next(node for node in ast.parse(utils_source).body if isinstance(node,ast.FunctionDef) and node.name == 'ValidateFolderAccessible')
exec(compile(ast.Module(body=[folder_function],type_ignores=[]),'upstream/utils.py','exec',flags=__future__.annotations.compiler_flag),env)
utils_module.ValidateFolderAccessible = ValidateFolderAccessible
edcb_module = ModuleType('app.utils.edcb.EDCBUtil')
async def edcb_status(url):
    calls.append('edcb:'+str(url))
    return scenario.get('edcb_status','Normal')
edcb_module.EDCBUtil = SimpleNamespace(getEDCBHost=lambda url:url.host,getEDCBPort=lambda url:url.port,getEDCBStatus=edcb_status)
sys.modules['app.utils.edcb.EDCBUtil'] = edcb_module
platform.machine = lambda:scenario.get('arch','AMD64')
def run_encoder(args, **kwargs):
    calls.append('encoder:'+args[0]+':'+args[1])
    if scenario.get('encoder_error'):
        raise FileNotFoundError('Encoder not found')
    return SimpleNamespace(stdout=scenario.get('encoder_stdout','H.264/AVC H.265/HEVC').encode('utf-8'))
subprocess.run = run_encoder
def get_mirakurun(url, **kwargs):
    calls.append('http:'+str(url))
    if scenario.get('mirakurun_error'):
        raise httpx.ConnectError('Connection refused')
    return httpx.Response(scenario.get('mirakurun_status',200),headers={'server':scenario.get('mirakurun_server','mirakc/3.4.4')},content=scenario.get('mirakurun_body','[]'))
httpx.get = get_mirakurun
def process(pid=None):
    if pid is None:
        return SimpleNamespace(pid=100,ppid=lambda:50)
    if str(pid) not in scenario.get('parents',{}):
        raise ProcessLookupError()
    return SimpleNamespace(pid=pid,ppid=lambda:scenario['parents'][str(pid)])
psutil = SimpleNamespace(Process=process,net_connections=lambda:[SimpleNamespace(status='LISTEN',pid=item.get('pid'),laddr=SimpleNamespace(port=item['port'])) for item in scenario.get('listeners',[])])
real_stat = Path.stat
def stat_path(path, *args, **kwargs):
    if str(path) == 'denied':
        raise PermissionError('Denied')
    return real_stat(path,*args,**kwargs)
Path.stat = stat_path

app = FastAPI()
async def GetCurrentAdminUser(request:Request):
    if request.headers.get('x-test-deny') == '1':
        raise HTTPException(401,'Not authenticated')
    return object()
@app.put('/server')
def validate_server(settings:Annotated[ServerSettings,Body()],user=Depends(GetCurrentAdminUser)):
    return Response(settings.model_dump_json(),media_type='application/json')
client = TestClient(app,raise_server_exceptions=False)
cases = []
def add(name,value=None,raw=None,options=None,bypass=False):
    global scenario
    scenario = options or {}
    calls.clear()
    raw = json.dumps(value,ensure_ascii=False) if raw is None else raw
    try:
        model = ServerSettings.model_validate(json.loads(raw),context={'bypass_validation':bypass},from_attributes=True)
        model_result = dict(status=200,normalized=json.loads(model.model_dump_json()),body=model.model_dump_json())
    except ValidationError as error:
        # FastAPI の例外処理と同じエラー変換 (ValueError の ctx は空の辞書)。
        try:
            model_result = dict(status=422,detail=json.loads(json.dumps(error.errors(include_url=False),ensure_ascii=False,default=lambda value:{},allow_nan=False)))
            for item in model_result['detail']:
                item['loc'].insert(0,'body')
        except ValueError:
            model_result = dict(status=500)
    except Exception:
        model_result = dict(status=500)
    model_result['calls'] = list(calls)
    cases.append(dict(name=name,input=raw,options=scenario,bypass=bypass,**model_result))

with tempfile.TemporaryDirectory() as directory:
    previous = os.getcwd(); os.chdir(directory)
    Path('settings-dir').mkdir(); Path('settings-file.ts').write_bytes(b'ts')
    add('defaults',{})
    add('unknown sections',{'iptv':{'enabled':False},'unknown':1})
    add('multiple errors in declared order',{'capture':{'upload_folders_polling_interval':-1},'video':{'recorded_folders':['missing',False],'recorded_folders_polling_interval':'5'},'server':{'port':0},'general':{'encoder':'invalid','mirakurun_url':True,'backend':'invalid'}})
    add('earlier model error followed by fatal encoder error',{'general':{'backend':'invalid','encoder':'FFmpeg'},'video':{'recorded_folders':['denied']}},options={'encoder_error':True})
    for value in [None,False,[],0,'hello']:
        add('root/'+repr(value),value)
    values = [None,False,True,0,1,-1,1.25,'','false','TRUE','yes','2','1_000','abc',[],{},['x'],[1],float('nan'),float('inf')]
    for section, field in ServerSettings.model_fields.items():
        add(section+'/null',{section:None})
        for name in field.annotation.model_fields:
            for i,value in enumerate(values):
                add(f'{section}/{name}/{i}',{section:{name:value}})
    for field in ['edcb_url','mirakurun_url']:
        for url in ['tcp://host:4510','TCP://Host:0/a/../b','tcp://edcb-namedpipe','tcp:/host','tcp:host','tcp:///','tcp://','http://Example.COM:80/a/../b?x=y#fragment','http://ユーザー:パス@日本語.jp','http://127.1','http://0x7f000001','http://[::1]:40772','http://a:65536','http://a:no','http://[wrong]','http://a b','http://a%00','http:','https://','file:///a','ftp://a','relative/path','  http://EXAMPLE.com/ \n','http:\\\\host\\a','http://a:','http://a/#a/','tcp://Höst:4510']:
            add(f'url/{field}/{url!r}',{'general':{field:url}},bypass=True)
        for url in ['http://256.1.2.3','http://1.2.3.4.5','http://1.2.3.no','http://[::1','tcp://a b:4510','tcp://:4510','tcp://user@:4510','http://%','http://xn--','http://a:-1','http://a:0','http://a:00080','\x00http://a\x00','http://a/%2e%2e/b','tcp://[::ffff:127.0.0.1]:4510','http://[1::2::3]']:
            add(f'url/edge/{field}/{url!r}',{'general':{field:url}},bypass=True)
    for section,name in [('server','custom_https_certificate'),('server','custom_https_private_key'),('tv','debug_mode_ts_path'),('video','recorded_folders'),('capture','upload_folders')]:
        for path in ['','.', './settings-dir//./','settings-dir/../settings-dir','missing','settings-file.ts','denied','with\x00null']:
            value = [path] if name.endswith('folders') else path
            add(f'path/{section}/{name}/{path!r}',{section:{name:value}})
    for options in [{},{'edcb_status':'Unknown'},{'edcb_status':'Recording'},{'edcb_status':'EPGGathering'}]:
        add('edcb/status/'+str(options),{'general':{'edcb_url':'tcp://host:4510'}},options=options)
    for general in [{'backend':'invalid','edcb_url':'tcp://host'}, {'backend':'Mirakurun','edcb_url':'tcp://host'}, {'edcb_url':'tcp://host'}, {'edcb_url':'tcp://edcb-namedpipe'}, {'backend':'Mirakurun','mirakurun_url':'http://host/base///'}, {'always_receive_tv_from_mirakurun':'yes','mirakurun_url':'http://host'}, {'backend':'bad','always_receive_tv_from_mirakurun':True,'mirakurun_url':'http://host'}]:
        add('backend/dependencies/'+repr(general),{'general':general})
    for options in [{},{'mirakurun_error':True},{'mirakurun_status':503},{'mirakurun_body':'{}'},{'mirakurun_body':'null'},{'mirakurun_body':'broken'},{'mirakurun_server':''},{'mirakurun_server':'Mirakurun'},{'mirakurun_server':'Mirakurun/3.9.0-rc.4'},{'mirakurun_server':'MIRAKC/'},{'mirakurun_server':'foo/bar'}]:
        add('mirakurun/response/'+repr(options),{'general':{'backend':'Mirakurun','mirakurun_url':'http://host'}},options=options)
    for arch in ['AMD64','x86_64','aarch64','other']:
        for encoder in LIBRARY_PATH:
            add(f'encoder/{arch}/{encoder}',{'general':{'encoder':encoder}},options={'arch':arch})
    for stdout in ['unavailable.','reader: H.264/AVC unavailable.\nH.265/HEVC','unavailable','H.264/AVC','']:
        add('encoder/output/'+repr(stdout),{'general':{'encoder':'QSVEncC'}},options={'encoder_stdout':stdout})
    add('encoder/missing',{'general':{'encoder':'FFmpeg'}},options={'encoder_error':True})
    for port in [0,1,1023,1024,65525,65526,99999999999999999999999999]:
        add('port/range/'+str(port),{'server':{'port':port}})
        add('port/bypass/'+str(port),{'server':{'port':port}},bypass=True)
    for pid,parent in [(100,50),(50,1),(101,100),(102,50),(103,99),(104,None),(None,None)]:
        for port in [7000,7010,7001]:
            options={'listeners':[{'pid':pid,'port':port}], 'parents':{str(pid):parent} if parent is not None else {}}
            add(f'port/owner/{pid}/{parent}/{port}',{'server':{'port':7000}},options=options)
    # 実際の FastAPI の本文処理と認証の順序も固定する。
    unauthorized=[]
    for raw in ['{','null','{"server":{"port":false}}','{}']:
        response=client.put('/server',content=raw,headers={'content-type':'application/json','x-test-deny':'1'})
        unauthorized.append(dict(input=raw,status=response.status_code,body=response.text))
    os.chdir(previous)

fixture=dict(upstream_commit=REF,source_sha256=hashlib.sha256(config_source.encode()).hexdigest(),utils_sha256=hashlib.sha256(utils_source.encode()).hexdigest(),runtime=RUNTIME,defaults=json.loads(ServerSettings().model_dump_json()),cases=cases,unauthorized=unauthorized)
target=ROOT/'server-go/internal/config/testdata/server_settings_validation_reference.json'
target.write_text(json.dumps(fixture,ensure_ascii=False,indent=2)+'\n',encoding='utf-8')
print(f'{len(cases)} ServerSettings cases -> {target}')
