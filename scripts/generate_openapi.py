#!/usr/bin/env python3
"""根据随仓 Engine 契约生成 Swagger OpenAPI 文档；--check 检查是否同步。"""
import json
from pathlib import Path
import sys
root = Path(__file__).resolve().parents[1]
spec = json.loads((root / 'contracts/protocol.json').read_text())
limits = spec['limits']
def obj(properties, required=None, strict=False):
    s = {'type':'object','properties':properties}
    if required: s['required']=required
    if strict: s['additionalProperties']=False
    return s
def string(**kw): return dict(type='string', **kw)
def infer(v):
    if isinstance(v, bool): return {'type':'boolean'}
    if isinstance(v, int): return {'type':'integer'}
    if isinstance(v, str): return string()
    if isinstance(v, list): return {'type':'array','items':infer(v[0]) if v else {}}
    return obj({k:infer(x) for k,x in v.items()}, list(v))
def text_limit(key): return string(minLength=1, description=f'非空文字，UTF-8 最多 {limits[key]} 字节。')
message=obj({'role':string(enum=['system','user','assistant']),'content':text_limit('chat_message_bytes')},['role','content'],True)
format_schema=obj({'type':string(enum=['json_object','text'])},['type'],True)
requests={
 'chat':obj({'model':string(description='选择 models 列表中的模型；管理员未开启选择时使用默认模型。'),'messages':{'type':'array','minItems':1,'maxItems':limits['chat_messages'],'items':message},'stream':{'type':'boolean','enum':[False],'default':False},'max_tokens':{'type':'integer','minimum':0,'maximum':limits['chat_max_tokens'],'default':limits['chat_default_tokens'],'description':'省略或 0 使用服务端默认值。'},'temperature':{'type':'number','minimum':0,'maximum':2},'response_format':format_schema,'thinking':obj({'type':string(enum=['disabled'])},['type'],True),'enable_thinking':{'type':'boolean','enum':[False]}},['messages'],True),
 'translation':obj({'text':text_limit('translation_input_bytes'),'source_lang':string(pattern='^[A-Za-z-]{2,16}$',example='AUTO'),'target_lang':string(pattern='^[A-Za-z-]{2,16}$',example='EN')},['text','source_lang','target_lang'],True),
 'transcription':obj({'file':string(format='binary',description=f'PCM/IEEE-float RIFF/WAVE，最多 {limits["audio_file_bytes"]} 字节。'),'model':string(description='服务端覆盖此模型字段。'),'language':string(pattern='^[A-Za-z-]{2,16}$',example='zh'),'response_format':string(enum=['json'],default='json')},['file'],True)
}
summaries={'models':'查询可选聊天模型','health':'健康检查','capabilities':'查询已启用能力','cloud':'云候选','chat':'AI 联想 / 语音润色','translation':'候选翻译','transcription':'WAV 批量语音转写'}
paths={}
for key,op in spec['operations'].items():
    responses={'200':{'description':'成功','content':{'application/json':{'schema':infer(op['response']),'example':op['response']}}}}
    if op['authenticated']:
        for code,description in spec['statuses'].items():
            responses[code]={'description':description,'content':{'application/json':{'schema':{'$ref':'#/components/schemas/Error'}}}}
        for code in ['429','503']:
            responses[code]['headers']={'Retry-After':{'description':'重试等待秒数（繁忙时提供）','schema':{'type':'integer'}}}
    operation={'operationId':key,'summary':summaries[key],'tags':['系统' if key in ['health','capabilities'] else '在线输入'],'responses':responses}
    if not op['authenticated']:operation['security']=[]
    if key in requests:
        media={'schema':requests[key]}
        if 'request' in op:media['example']=op['request']
        operation['requestBody']={'required':True,'content':{op['content_type']:media}}
    if key=='cloud':
        operation['description']=op['description']+' 启用原生引擎时，无完整拼音云候选会补查覆盖整段输入的词典词条；Engine 确认拼写纠错且找到完整词条时优先采用词典结果；limit 为上限，不保证返回数量。'
        operation['parameters']=[{'name':name,'in':'query','required':name=='text','schema':schema} for name,schema in {'text':dict(text_limit('cloud_input_bytes'),example='haohaoxuexi',description='输入拼音，例如 haohaoxuexi 或 hao hao xue xi；不要输入已转换的中文。最多 256 UTF-8 字节。'),'scheme':string(enum=op['schemes'],default='pinyin'),'limit':{'type':'integer','minimum':1,'maximum':limits['cloud_candidates'],'default':5}}.items()]
    paths[op['path']]={op['method'].lower():operation}
for key,op in spec['websocket_operations'].items():
    paths[op['path']]={'get':{'operationId':key,'summary':'实时语音 WebSocket（仅文档）','tags':['实时语音'],'description':op['protocol']+'\n\nSwagger UI 不支持 WebSocket 二进制会话。使用 WSS 客户端携带设备 Bearer 令牌建立连接。'+op['close_policy']+f' 单消息最多 {limits["stream_message_bytes"]} 字节，每方向每会话最多 {limits["stream_session_bytes"]} 字节。','x-websocket':True,'responses':{'101':{'description':'WebSocket 升级成功；后续为豆包 ASR v1 二进制消息'},'400':{'description':'需要 WebSocket Upgrade'},'401':{'description':'设备令牌无效'},'503':{'description':'功能未启用或服务繁忙'}}}}
result={'openapi':'3.0.3','info':{'title':'水杉输入法后端 API','version':spec['version'],'description':'水杉输入法共通后端。点击 Authorize 填写设备令牌（不含 Bearer 前缀）。功能是否启用请查询 capabilities。JSON 请求最多 64 KiB，multipart 总体最多 16 MiB。服务不保存输入和音频。'},'servers':[{'url':'/'}],'security':[{'deviceToken':[]}],'paths':paths,'components':{'securitySchemes':{'deviceToken':{'type':'http','scheme':'bearer','description':'管理员发放的设备令牌；不是供应商密钥。'}},'schemas':{'Error':infer(spec['error_response'])}}}
# 用户体系独立于 Engine 输入协议，避免修改客户端共通契约。
user=obj({'id':string(),'display_name':string(),'created_at':string(format='date-time'),'email':string(format='email',description='已绑定 Google 身份的已验证邮箱，仅返回给用户本人；没有时省略。'),'avatar_url':string(format='uri',description='头像地址：上传的自定义头像优先，其次是 Google 头像；都没有时省略，客户端显示昵称首字。')},['id','display_name','created_at'])
tokens=obj({'access_token':string(),'refresh_token':string(),'token_type':string(enum=['Bearer']),'expires_in':{'type':'integer'},'user':user})
provider=string(enum=['apple','google','wechat','phone','email'])
auth_operations=[
 ('/v1/auth/providers','get','查询可用登录方式',None,obj({'providers':obj({p:{'type':'boolean'} for p in ['apple','google','wechat','phone','email']})}),False,200),
 ('/v1/auth/challenges','post','创建登录或绑定挑战',obj({'provider':provider,'target':string(description='邮箱地址或 E.164 手机号。Google 桌面端传本机回环回调地址 http://127.0.0.1:<端口>/callback 或 http://[::1]:<端口>/callback（端口 1024–65535），由服务端持有 PKCE 与客户端密钥换码，响应附带 authorization_url；地址不合规返回 invalid_target。其余第三方登录省略。'),'purpose':string(enum=['login','link'],default='login')},['provider'],True),obj({'challenge_id':string(),'expires_in':{'type':'integer'},'nonce':string(),'authorization_url':string()}),False,201),
 ('/v1/auth/login','post','验证凭据并登录或绑定',obj({'challenge_id':string(),'credential':string(description='六位验证码、Apple/Google ID Token、Google 桌面回环回调收到的授权 code，或微信授权 code。')},['challenge_id','credential'],True),tokens,False,200),
 ('/v1/auth/refresh','post','轮换用户会话令牌',obj({'refresh_token':string()},['refresh_token'],True),tokens,False,200),
 ('/v1/auth/logout','post','退出当前或全部会话',obj({'all':{'type':'boolean','default':False}},strict=True),None,True,204),
 ('/v1/users/me','get','查询当前用户和已绑定身份',None,obj({'user':user,'identities':{'type':'array','items':obj({'provider':provider,'subject':string()})}}),True,200),
 ('/v1/users/me','patch','修改当前用户昵称',obj({'display_name':string(maxLength=64)},['display_name'],True),None,True,204),
 ('/v1/users/me','delete','注销当前用户',None,None,True,204),
 ('/v1/users/me/avatar','delete','删除自定义头像',None,None,True,204),
]
for path,method,title,body,response,protected,status in auth_operations:
    responses={str(status):{'description':'成功'}}
    if response: responses[str(status)]['content']={'application/json':{'schema':response}}
    for code in ['400','401','403','409','415','429','503']:
        responses[code]={'description':'请求无效、凭据失效、需要重新登录、身份冲突、格式错误、限流或功能不可用。','content':{'application/json':{'schema':{'$ref':'#/components/schemas/Error'}}}}
    op={'summary':title,'tags':['用户体系'],'security':[{'userSession':[]}] if protected else [],'responses':responses,'description':'JSON 请求最多 16 KiB。绑定身份需要在挑战创建和验证时携带同一用户的会话令牌；绑定和注销要求最近 10 分钟内登录。设备令牌不能用于用户管理。'}
    if body: op['requestBody']={'required':True,'content':{'application/json':{'schema':body}}}
    paths.setdefault(path,{})[method]=op
# 并发刷新：轮换后 30 秒内再次出现的旧刷新令牌返回 409 refresh_superseded，不撤销会话。
paths['/v1/auth/refresh']['post']['description']+=' 刷新令牌轮换后 30 秒内再次提交旧令牌（并发刷新输掉竞争）时返回 409 refresh_superseded，会话不撤销，调用方应改用并发请求拿到的新令牌；超过 30 秒的重放返回 401 并撤销整个会话。'
paths['/v1/auth/refresh']['post']['responses']['409']=dict(paths['/v1/auth/refresh']['post']['responses']['409'],description='refresh_superseded：该刷新令牌 30 秒内刚被轮换，会话仍有效。')
paths['/v1/users/me/avatar']['put']={'summary':'上传自定义头像','tags':['用户体系'],'security':[{'userSession':[]}],'description':'请求体为 PNG 或 JPEG 原始字节，最多 1 MiB，边长不超过 4096。服务端裁成居中正方形并重新编码为 256×256 JPEG，存入公开存储并替换原有自定义头像；每用户每小时最多 20 次。未配置头像存储时返回 503。','requestBody':{'required':True,'content':{'image/png':{'schema':string(format='binary')},'image/jpeg':{'schema':string(format='binary')}}},'responses':{'200':{'description':'成功，返回更新后的用户和身份','content':{'application/json':{'schema':obj({'user':user,'identities':{'type':'array','items':obj({'provider':provider,'subject':string()})}})}}},'400':{'description':'不是有效的 PNG/JPEG 图片'},'401':{'description':'需要登录'},'413':{'description':'超过 1 MiB'},'415':{'description':'不是 image/png 或 image/jpeg'},'429':{'description':'限流'},'502':{'description':'头像存储不可用'},'503':{'description':'未配置头像存储'}}}
result['security']=[{'deviceToken':[]},{'userSession':[]}]
result['components']['securitySchemes']['userSession']={'type':'http','scheme':'bearer','description':'登录返回的 access_token，不是 refresh_token 或供应商密钥。'}
result['info']['description']+=' 用户接口详见用户体系标签；登录成功后也可使用用户 access_token 调用在线输入接口。'
# 跨端用户数据不属于 Engine 在线输入契约。
preference_fields=json.loads((root/'internal/account/preferences_fields.json').read_text())
settings=obj(preference_fields,strict=True)
preferences=obj({'revision':{'type':'integer','format':'int64','minimum':0},'settings':settings},['revision','settings'],True)
clipboard_item=obj({'id':string(),'text':string(description='最多 4000 个 UTF-16 单元，不允许空白或 NUL。'),'updated_at':string(format='date-time')})
shared_operations=[
 ('/v1/users/me/preferences','get','读取跨端偏好',None,preferences,200,'新用户返回 revision=0、空 settings。仅保存白名单字段，不保存凭据或本机路径。'),
 ('/v1/users/me/preferences','put','替换跨端偏好',preferences,preferences,200,'请求最多 1 MiB；revision 必须匹配当前版本，否则返回 409。成功后版本加一；未提交的字段被移除。'),
 ('/v1/users/me/preferences/schema','get','查询可同步偏好字段',None,obj({'fields':obj({},strict=False),'maximum_bytes':{'type':'integer'},'update_mode':string(enum=['replace']),'revision_required':{'type':'boolean'}}),200,'返回允许同步的字段及类型；不包含本机配置值。'),
 ('/v1/users/me/clipboard','get','查询和搜索云端剪贴板',None,obj({'enabled':{'type':'boolean'},'items':{'type':'array','maxItems':50,'items':clipboard_item}}),200,'按最近添加顺序返回最多 50 条。q 为大小写不敏感的原文子串，最多 1024 UTF-8 字节。'),
 ('/v1/users/me/clipboard','post','添加云端剪贴板条目',obj({'text':clipboard_item['properties']['text']},['text'],True),clipboard_item,200,'必须显式开启同步，否则返回 403。请求最多 32 KiB，文本最多 4000 个 UTF-16 单元。重复文本保留 ID 并移动到最前；超过 50 条移除最旧条目。'),
 ('/v1/users/me/clipboard','delete','清空云端剪贴板',None,None,204,'只清空当前用户云端记录，不操作客户端系统剪贴板。'),
 ('/v1/users/me/clipboard/{id}','delete','删除云端剪贴板条目',None,None,204,'条目不存在或属于其他用户均返回 404。'),
 ('/v1/users/me/clipboard/settings','put','开启或关闭云端剪贴板',obj({'enabled':{'type':'boolean'}},['enabled'],True),obj({'enabled':{'type':'boolean'}}),200,'默认关闭，必须由用户显式开启；关闭会删除该用户全部云端剪贴板记录。请求最多 16 KiB。'),
]
for path,method,title,body,response,status,description in shared_operations:
    responses={str(status):{'description':'成功'}}
    if response: responses[str(status)]['content']={'application/json':{'schema':response}}
    for code,reason in [('400','参数无效'),('401','需要有效用户会话'),('403','未开启剪贴板同步'),('404','条目不存在'),('409','偏好版本冲突'),('415','需要 application/json'),('503','用户数据服务不可用')]:
        responses[code]={'description':reason,'content':{'application/json':{'schema':{'$ref':'#/components/schemas/Error'}}}}
    op={'summary':title,'tags':['用户同步数据'],'description':description+' 设备令牌不能访问用户同步数据；注销账号会级联删除这些数据。','security':[{'userSession':[]}],'responses':responses}
    if body: op['requestBody']={'required':True,'content':{'application/json':{'schema':body}}}
    if '{id}' in path: op['parameters']=[{'name':'id','in':'path','required':True,'schema':string()}]
    if method=='get' and path.endswith('/clipboard'): op['parameters']=[{'name':'q','in':'query','schema':string()}]
    paths.setdefault(path,{})[method]=op
result['info']['description']=result['info']['description'].replace('服务不保存输入和音频。','在线输入接口不保存输入和音频；用户同步接口按用户操作保存偏好及显式上传的数据。')
# 无状态 Engine 查询；算法及发布词库由原生公共库提供。
candidate=obj({'code':string(),'canonical_pinyin':string(),'word':string(),'weight':{'type':'integer','format':'int64'},'fixed_position':{'type':'integer'}})
candidate_response=obj({'candidates':{'type':'array','items':candidate},'raw_segmentation':string(),'normalized_segmentation':string()})
paths['/v1/input/capabilities']={'get':{'summary':'查询公共引擎配置能力','tags':['公共输入引擎'],'description':'返回 Engine 和词库是否配置，以及支持的输入方案、双拼方案和候选上限。','responses':{'200':{'description':'成功'},'401':{'description':'缺少有效令牌'}}}}
input_titles={'romaji':'日语罗马字与假名转换','japanese':'日语罗马字候选查询','convert':'简体转繁体（OpenCC s2t）','annotate':'纯汉字词组注音','unicode':'Unicode 码点候选','datetime':'日期时间候选','english':'英文前缀补全','gloss':'中英双向释义','emoji':'Emoji 拼音查询','kaomoji':'颜文字拼音查询','jianpin':'简拼候选','candidates':'本地词库候选','segmentation':'输入方案切分','quick':'快捷短语候选','helpcode':'汉字辅助码'}
for operation,title in input_titles.items():
    fields={'text':string(minLength=1,description='查询文字；输入码最多 256 ASCII 字符，其他文字最多 8192 UTF-8 字节。'),'limit':{'type':'integer','minimum':1,'maximum':200,'default':20}}
    if operation in ['emoji','kaomoji','jianpin','candidates','segmentation']:
        fields.update({'scheme':string(enum=['pinyin','shuangpin','wubi'],default='pinyin'),'profile':string(enum=['xiaohe','ziranma','shoudao','microsoft'],default='xiaohe')})
    if operation=='datetime': fields.update({'time':string(format='date-time',description='RFC 3339 参考时刻，省略使用当前时间。'),'timezone':string(default='UTC',example='Asia/Shanghai',description='IANA 时区。')})
    if operation=='gloss': fields['direction']=string(enum=['en-zh','zh-en'],default='en-zh')
    if operation=='romaji': fields['direction']=string(enum=['romaji-hiragana','hiragana-katakana','kana-romaji'],default='romaji-hiragana')
    if operation=='helpcode': fields['schema']=string(enum=['lantian','ziranma','shouyou2_0','shouyouplus','xiaohe'],default='lantian')
    response=candidate_response
    if operation=='romaji': response=obj({'text':string(),'pending':string(),'complete':{'type':'boolean'}})
    if operation=='japanese': response=obj(dict(candidate_response['properties'],hiragana=string(),pending=string(),complete={'type':'boolean'}))
    if operation in ['gloss','helpcode','convert']: response=obj({'text':string(),'schema':string(),'conversion':string()})
    if operation=='annotate': response=obj({'code':string(),'word':string()})
    if operation=='segmentation': response=obj({'raw':string(),'normalized':string()})
    responses={'200':{'description':'成功','content':{'application/json':{'schema':response}}}}
    for code,description in [('400','参数无效'),('401','缺少有效设备或用户令牌'),('429','限流'),('502','原生查询失败'),('503','原生 Engine 或数据未配置，或服务繁忙'),('504','查询超时')]: responses[code]={'description':description}
    paths['/v1/input/'+operation]={'post':{'summary':title,'tags':['公共输入引擎'],'description':'复用与客户端同版本的输入引擎，无状态查询，不写入用户学习记录。请求最多 64 KiB；不接受资源路径、运行命令或上游地址。','requestBody':{'required':True,'content':{'application/json':{'schema':obj(fields,['text'],True)}}},'responses':responses}}
for kind,title in [('emoji','Emoji'),('kaomoji','颜文字'),('symbols','符号')]:
    paths['/v1/catalog/'+kind]={'get':{'summary':title+'目录与分类','tags':['公共输入引擎'],'description':'返回按发布词库顺序排列的条目、全部分类及数量；q 搜索文字或关键词，category 精确匹配分类。','parameters':[{'name':k,'in':'query','schema':v} for k,v in {'q':string(),'category':string(),'offset':{'type':'integer','minimum':0,'maximum':1000000,'default':0},'limit':{'type':'integer','minimum':1,'maximum':200,'default':50}}.items()],'responses':{'200':{'description':'成功','content':{'application/json':{'schema':obj({'items':{'type':'array','items':obj({'text':string(),'category':string(),'parent_category':string(),'keywords':string()})},'categories':{'type':'array','items':obj({'name':string(),'parent':string(),'count':{'type':'integer'}})},'offset':{'type':'integer'},'has_more':{'type':'boolean'}})}}},'400':{'description':'参数无效'},'401':{'description':'缺少有效令牌'},'503':{'description':'词库不可用'}}}}
entry=obj({'id':string(),'kind':string(enum=['pinyin','wubi','english','quick']),'code':string(),'word':string(),'weight':{'type':'integer','format':'int64'},'revision':{'type':'integer','format':'int64'},'updated_at':string(format='date-time')})
position_fields={'context':string(description='Engine 返回的固定位置上下文，最多 512 UTF-8 字节。'),'code':string(description='候选规范编码，最多 512 UTF-8 字节。'),'word':string(description='候选文字，最多 2048 UTF-8 字节。')}
position=obj(dict(position_fields,position={'type':'integer','minimum':0,'maximum':5}),['context','code','word','position'])
selection=obj({'context':string(),'code':string(),'word':string(),'count':{'type':'integer','minimum':0,'maximum':10}})
entry['properties']['user_inserted']={'type':'boolean','description':'省略表示用户新增；false 表示基础候选的调频覆盖。'}
change=obj({'reset':{'type':'boolean','description':'true 表示完整状态已被替换；客户端应丢弃词库缓存并重新读取完整快照。'},'ranking':{'type':'array','items':entry},'selection':selection,'position':position,'revision':{'type':'integer','format':'int64'},'previous':dict(entry,nullable=True),'replacement':dict(entry,nullable=True)})
entry_body=obj({'code':string(),'word':string(),'weight':{'type':'integer','format':'int64','minimum':0,'default':10}},['code','word'],True)
update_body=obj(dict(entry_body['properties'],revision={'type':'integer','format':'int64','minimum':1}),['code','word','revision'],True)
page_params=[{'name':'offset','in':'query','schema':{'type':'integer','minimum':0,'maximum':1000000,'default':0}},{'name':'limit','in':'query','schema':{'type':'integer','minimum':1,'maximum':200,'default':200}}]
dictionary_ops=[
 ('/v1/users/me/dictionary/positions','get','查询用户固定候选位置',None,obj({'positions':{'type':'array','items':position},'has_more':{'type':'boolean'},'offset':{'type':'integer'}}),200,'按上下文、位置排序；context 可选精确筛选。'),
 ('/v1/users/me/dictionary/positions','put','设置固定候选位置',obj(dict(position_fields,revision={'type':'integer','format':'int64','minimum':0},position={'type':'integer','minimum':1,'maximum':5}),['revision','context','code','word','position'],True),obj({'revision':{'type':'integer','format':'int64'}}),200,'revision 必须匹配用户词库总版本。每个上下文五个位置；占用同一位置会替换旧设置。同一候选移动时释放旧位置。context、code、word 总计最多 2048 UTF-8 字节。'),
 ('/v1/users/me/dictionary/positions','delete','清除固定候选位置',obj(dict(position_fields,revision={'type':'integer','format':'int64','minimum':0}),['revision','context','code','word'],True),obj({'revision':{'type':'integer','format':'int64'}}),200,'revision 必须匹配用户词库总版本；清除记录以 position=0 写入变更日志。context、code、word 总计最多 2048 UTF-8 字节。'),
 ('/v1/users/me/dictionary/candidates','post','查询个人词库与基础词库合并候选',obj({'text':string(),'kind':string(enum=['pinyin','wubi','english','quick','jianpin'],default='pinyin'),'scheme':string(enum=['pinyin','shuangpin','wubi'],default='pinyin'),'profile':string(enum=['xiaohe','ziranma','shoudao','microsoft'],default='xiaohe'),'limit':{'type':'integer','minimum':1,'maximum':200,'default':20}},['text'],True),obj(dict(candidate_response['properties'],context=string(description='固定位置操作使用的上下文键。'),revision={'type':'integer','format':'int64'})),200,'同一数据库快照读取当前用户覆盖及 revision，再由 Engine 将覆盖和删除记录回放到临时词库副本。只读公共基础词库；副本在查询完成或失败后清理；返回完整词库版本以供客户端判断缓存。'),
 ('/v1/users/me/dictionaries/{kind}/import-hans','post','纯汉字词组注音导入',obj({'text':string(description='每行一个纯汉字词组，最多 128 个汉字。'),'weight':{'type':'integer','minimum':0,'default':10}},['text'],True),obj({'imported':{'type':'integer'},'revision':{'type':'integer','format':'int64'}}),200,'仅支持 pinyin 类别；1–500 个词组、JSON 最多 64 KiB；沿用 cpp-pinyin 词组注音并经 Engine 校验，任何失败均不写入。'),
 ('/v1/users/me/dictionaries/{kind}','get','查询个人词条',None,obj({'entries':{'type':'array','items':entry},'has_more':{'type':'boolean'},'offset':{'type':'integer'}}),200,'支持 q 原文子串搜索，最多 1024 UTF-8 字节；按编码和文字稳定排序。只查询当前用户创建的词条。'),
 ('/v1/users/me/dictionaries/{kind}','post','新增个人词条',entry_body,change,201,'复用 Engine 规范化与校验；同类编码和文字重复返回 409；每个用户最多 100000 个词条。'),
 ('/v1/users/me/dictionaries/{kind}/{id}','put','修改个人词条',update_body,change,200,'revision 必须匹配该词条版本，否则返回 409；不存在或属于其他用户均返回 404。'),
 ('/v1/users/me/dictionaries/{kind}/{id}','delete','删除个人词条',obj({'revision':{'type':'integer','format':'int64','minimum':1}},['revision'],True),change,200,'要求该词条当前 revision；删除保留变更记录以供其他设备同步。'),
 ('/v1/users/me/dictionaries/{kind}/import','post','批量导入个人词条',obj({'text':string(description='三列 TSV，权重沿用 Engine 的 1–100000000 范围。'),'format':string(enum=['standard','windows'],default='standard')},['text'],True),obj({'imported':{'type':'integer'},'revision':{'type':'integer','format':'int64'}}),200,'JSON 最多 64 KiB，一次 1–500 条。standard 三列为文字、编码、权重；windows 的英文和快捷短语三列为编码、文字、权重，拼音和五笔为文字、编码、权重。权重必须为 Engine 支持的 1–100000000，不接受旧文件中的零权重；任何无效、重复或超配额均全部回滚。'),
 ('/v1/users/me/dictionaries/{kind}/export','get','导出个人词条',None,None,200,'standard 导出当前用户新增词条，列为文字、编码、权重。windows 匹配 Windows 导出列顺序：英文和快捷短语为编码、文字、权重；拼音和五笔为文字、编码、权重。windows 拼音导出所有多字 upsert 覆盖（包含调频），其余类别仅导出用户新增词条；均排除删除记录。一条 SELECT 取得一致快照。'),
 ('/v1/users/me/dictionary/changes','get','读取个人词库增量变更',None,obj({'changes':{'type':'array','items':change},'next':{'type':'integer','format':'int64'},'has_more':{'type':'boolean'}}),200,'after 为已消费的用户词库版本，默认 0；返回更大版本的有序变更，next 可用于继续读取。删除记录 replacement 为 null。')
]
ranking_query=next(body for path,method,title,body,response,status,description in dictionary_ops if path.endswith('/dictionary/candidates'))
ranking_action=obj({'code':string(),'word':string(),'mode':string(enum=['disabled','pin','halve','linear','promote'],default='pin'),'linear_step':{'type':'integer','minimum':1,'maximum':100,'default':1},'trigger_count':{'type':'integer','minimum':1,'maximum':10,'default':1},'force_top':{'type':'boolean','default':False}},['code','word'],True)
dictionary_ops.append(('/v1/users/me/dictionary/ranking','post','调整用户候选排序',obj({'revision':{'type':'integer','format':'int64','minimum':0},'query':ranking_query,'action':ranking_action},['revision','query','action'],True),obj({'updates':{'type':'array','items':entry},'selection':selection,'changed':{'type':'boolean'},'revision':{'type':'integer','format':'int64'}}),200,'沿用 Engine 调频算法；仅支持拼音、双拼、五笔、简拼与英文。code、word 必须匹配当前候选，合计最多 1536 UTF-8 字节。revision 为用户词库总版本；每个成功操作递增版本，包括未达到触发次数的选择。计数和权重在同一用户事务保存，设备令牌不能调用；基础候选的权重覆盖不成为个人新增词条。'))
dictionary_ops.append(('/v1/users/me/dictionary/candidates','delete','删除当前用户的候选',obj({'revision':{'type':'integer','format':'int64','minimum':0},'query':ranking_query,'code':string(),'word':string()},['revision','query','code','word'],True),change,200,'精确匹配当前合并候选的编码和文字，调用 Engine 删除事务并保存当前用户删除记录；不修改公共词库。支持拼音、双拼、五笔、简拼和英文；非英文单字沿用 Windows 保护规则，不能删除。code 与 word 合计最多 1536 UTF-8 字节；revision 必须匹配用户词库总版本。删除用户新增候选时一并移除个人词条。'))
dictionary_ops.append(('/v1/users/me/dictionary/snapshot','get','导出完整用户词库状态',None,None,200,'从单条数据库查询的一致快照流式导出 NDJSON。header 包含 format=msime-dictionary-snapshot、version=1 和用户词库总 revision；后续 entry、overlay（含 deleted）、position、selection 记录保存个人词条、权重覆盖与删除、固定位置、触发计数。最后 footer 的 records 是此前记录数，sha256 是此前所有行（包含每行末尾 LF）的 SHA-256；没有有效 footer 的下载不完整。文件不含用户账号标识、会话或供应商凭据。'))
dictionary_ops.append(('/v1/users/me/dictionary/snapshot','put','原子恢复完整用户词库状态',string(format='binary'),obj({'revision':{'type':'integer','format':'int64'},'reset':{'type':'boolean','enum':[True]}}),200,'上传完整导出 NDJSON 文件，服务端检查记录格式、完整性和 Engine 词条规则后原子替换当前用户词库。revision 查询参数必须匹配目标用户当前总版本；源文件版本不能代替此参数。成功后生成新词条 ID，并写入 reset 变更，客户端需重新同步。单次最多 512 MiB、最多 100000 个人词条，处理期限 120 秒；每用户每分钟最多 5 次尝试，每服务进程同时处理一次恢复。任何失败都不改变目标用户状态。'))
dictionary_ops.append(('/v1/users/me/dictionaries/{kind}/catalog','get','分页查询基础词库与个人覆盖',None,obj({'entries':{'type':'array','items':obj({'kind':string(),'code':string(),'word':string(),'weight':{'type':'integer','format':'int64'}})},'offset':{'type':'integer'},'has_more':{'type':'boolean'},'revision':{'type':'integer','format':'int64'},'normalized':string()}),200,'查询当前用户与基础词库合并后的管理条目，包含调频覆盖并排除删除记录。拼音按 Engine 全拼/双拼规范编码精确查询；英文、五笔和快捷短语按前缀查询。仅快捷短语允许空 q 查询全部；按 Windows 管理器的权重及编码顺序分页，排序不应用候选固定位置。'))
dictionary_ops.append(('/v1/users/me/dictionaries/{kind}/edit','post','显式编辑合并词库中的词条',obj({'revision':{'type':'integer','format':'int64','minimum':0},'previous':obj({'code':string(),'word':string()},['code','word'],True),'replacement':dict(obj({'code':string(),'word':string(),'weight':{'type':'integer','format':'int64','minimum':1,'maximum':100000000}},['code','word','weight'],True),nullable=True)},['revision','previous','replacement'],True),change,200,'使用管理目录返回的精确 code 和 word 定位条目；revision 为目标用户当前词库总版本。replacement 为新编码、文字和权重；显式 null 表示删除，包括管理器中的单字条目。更改为已存在的编码和文字组合返回 409。个人新增词条保留 ID；基础词条只形成当前用户覆盖，不写公共词库，也不变成个人新增词条。所有变更写入增量记录和完整快照。'))
for path,method,title,body,response,status,description in dictionary_ops:
    parameters=[]
    if '{kind}' in path: parameters.append({'name':'kind','in':'path','required':True,'schema':string(enum=['pinyin','wubi','english','quick'])})
    if '{id}' in path: parameters.append({'name':'id','in':'path','required':True,'schema':string()})
    if method=='get' and path.endswith('{kind}'): parameters+=page_params+[{'name':'q','in':'query','schema':string()}]
    if path.endswith('/positions') and method=='get': parameters+=page_params+[{'name':'context','in':'query','schema':string()}]
    if path.endswith('/catalog'):
        parameters+=page_params+[{'name':'q','in':'query','schema':string()},{'name':'scheme','in':'query','schema':string(enum=['pinyin','shuangpin'],default='pinyin')},{'name':'profile','in':'query','schema':string(enum=['xiaohe','ziranma','shoudao','microsoft'],default='xiaohe')}]
    if path.endswith('/changes'): parameters+=[page_params[1],{'name':'after','in':'query','schema':{'type':'integer','format':'int64','minimum':0,'default':0}}]
    responses={str(status):{'description':'成功'}}
    if response: responses[str(status)]['content']={'application/json':{'schema':response}}
    if path.endswith('/snapshot') and method=='put':
        parameters.append({'name':'revision','in':'query','required':True,'schema':{'type':'integer','format':'int64','minimum':0}})
        responses['413']={'description':'快照超过 512 MiB'}
    if path.endswith('/snapshot') and method=='get': responses['200']['content']={'application/x-ndjson':{'schema':string()}}
    if path.endswith('/export'): parameters.append({'name':'format','in':'query','schema':string(enum=['standard','windows'],default='standard')})
    if path.endswith('/export'): responses['200']['content']={'text/plain':{'schema':string()}}
    for code,reason in [('400','参数或词条无效'),('401','需要有效用户会话'),('404','类别或词条不存在'),('409','版本冲突、重复词条或用户配额已满'),('415','需要 application/json'),('429','限流'),('502','Engine 查询失败'),('503','Engine 或用户数据服务不可用'),('504','操作超时')]: responses[code]={'description':reason}
    operation={'summary':title,'tags':['用户词库'],'security':[{'userSession':[]}],'description':description+' 原生校验沿用公共 Engine 规则；快捷短语最多 199 个 UTF-16 单元。设备令牌不能访问用户词库。','responses':responses,'parameters':parameters}
    if body: operation['requestBody']={'required':True,'content':{'application/json':{'schema':body}}}
    if path.endswith('/snapshot') and method=='put':
        operation['requestBody']['content']={'application/x-ndjson':{'schema':body}}
        operation['responses']['415']={'description':'需要 application/x-ndjson'}
        operation['responses']['503']={'description':'恢复忙碌或用户数据服务不可用；忙碌时返回 Retry-After: 5'}
    paths.setdefault(path,{})[method]=operation
skin_resource=obj({'path':string(),'size':{'type':'integer'},'sha256':string(),'media_type':string(),'url':string()})
skin_colors=obj({k:string() for k in ['accent','selected','hover','surface','border','text','number','translation']}|{'show_selected_bar':{'type':'boolean'}})
toolbar_colors=obj({k:string() for k in ['background','border','handle','divider','icon','hover']})
client_only='仅数据库皮肤包（客户端方言）提供；'
skin=obj({'schema_version':{'type':'integer','enum':[1]},'id':string(),'name':string(),'version':string(),'author':string(),'description':string(),'base':string(enum=['fluent','wechat','graphite','willow_green','system','shuishan','light','paper','night','ink'],description='内置与 skins_root 皮肤包沿用 Windows 方言，base 为四个内置 ID 之一；数据库皮肤包沿用客户端与 msime-windows 统一后的规则，base 为 system 或内置全局主题；清单中的 msime-windows 内置外观（fluent、wechat、graphite、willow_green、autumn_osmanthus、microsoft）返回 system，外观配色由客户端读清单时补齐，接口按清单原样返回颜色。'),'builtin':{'type':'boolean'},'toolbar_stylesheet':string(),'preview':string(),'supports':obj({'layouts':{'type':'array','items':string(enum=['horizontal','vertical'])},'themes':{'type':'array','items':string(enum=['dark','light'])}}),'candidate_window':obj({'min_width_dip':{'type':'number'},'corner_radius_dip':{'type':'number','minimum':0,'maximum':32,'description':client_only+'省略时沿用宿主圆角。'},'decoration':obj({'top_inset_dip':{'type':'number'},'width_dip':{'type':'number'},'image':string(description=client_only+'装饰图的包内路径；省略时客户端在有装饰带时改用 preview 图片。'),'align':string(enum=['left','center','right'],description=client_only+'默认 right。')}),'background':obj({'image':string(),'fit':string(enum=['cover','contain','stretch']),'opacity':{'type':'number','minimum':0,'maximum':1}},['image','fit','opacity'])}),'candidate':obj({'dark':skin_colors,'light':skin_colors}),'toolbar':obj({'corner_radius_dip':{'type':'number','minimum':0,'maximum':32},'dark':toolbar_colors,'light':toolbar_colors}),'license':obj({'code':string(),'assets':string(),'source':string()}),'resources':{'type':'array','items':skin_resource}})
skin_paths=[
 ('/v1/skins','内置和自定义皮肤目录',obj({'skins':{'type':'array','items':skin},'invalid_packages':{'type':'integer'},'license_url':string(),'source_url':string()})),
 ('/v1/skins/{id}','皮肤元数据和资源清单',skin),
 ('/v1/skins/source','内置皮肤固定来源与文件摘要',{'type':'object'}),
 ('/v1/skins/license','内置皮肤许可证',None),
 ('/v1/skins/{id}/resources/{resource}','下载皮肤资源',None)
]
for path,title,response in skin_paths:
    parameters=[]
    if '{id}' in path: parameters.append({'name':'id','in':'path','required':True,'schema':string(pattern='^[a-z0-9][a-z0-9._-]{0,63}$')})
    if '{resource}' in path: parameters.append({'name':'resource','in':'path','required':True,'schema':string(description='皮肤内相对路径，可包含子目录；仅返回资源清单中的 CSS、图片、字体及 skin.toml。')})
    if path=='/v1/skins': parameters=[{'name':'layout','in':'query','schema':string(enum=['horizontal','vertical'])},{'name':'theme','in':'query','schema':string(enum=['dark','light'])}]
    success={'description':'成功'}
    if response: success['content']={'application/json':{'schema':response}}
    else: success['content']={'text/plain' if path.endswith('/license') else 'application/octet-stream':{'schema':string()}}
    paths[path]={'get':{'summary':title,'tags':['皮肤'],'parameters':parameters,'description':'需要设备或用户令牌；内置皮肤随服务提供，自定义目录由管理员 skins_root 配置，启用用户体系时再并入数据库中已发布的候选框皮肤包（按客户端 msime-skins 规则逐次校验）。同一 ID 同时出现在 skins_root 和数据库时两边都不提供，计 1 个 invalid_packages；数据库不可用时返回 503。无效皮肤不进入列表，计入 invalid_packages；不暴露服务器路径或解析错误详情。单资源最多 4 MiB，单包最多 16 MiB、512 个目录条目。下载保留原始文件字节与摘要，客户端仍使用其皮肤 CSS 隔离规则。','responses':{'200':success,'400':{'description':'筛选参数无效'},'401':{'description':'缺少有效令牌'},'404':{'description':'皮肤、资源不存在或不安全'},'503':{'description':'皮肤目录不可用'}}}}
# User-created Apple keyboard designs are separate from the desktop CSS catalog.
community_design = obj({
    **{key: {'type':'integer','minimum':0,'maximum':16777215} for key in ['background','keyBackground','keyForeground','accent','actionBackground','gradientEnd','customBorderColor']},
    **{key: {'type':'number','minimum':lo,'maximum':hi} for key,lo,hi in [('cornerRadius',0,20),('borderWidth',0,2),('shadow',0,.4),('keyOpacity',.25,1),('patternOpacity',0,.5),('photoShade',0,.8),('photoPosition',0,1)]},
    'keyShape': {'type':'string','enum':['rounded','capsule','ticket','pebble']},
    'keyMaterial': {'type':'string','enum':['flat','raised','glass','paper']},
    'pattern': {'type':'integer','minimum':0,'maximum':3}, 'monospaced':{'type':'boolean'}, 'gradientHorizontal':{'type':'boolean'},
    'photo':{'type':'string','format':'byte','description':'JPEG, at most 512000 decoded bytes, at most 1024 pixels per axis'}
}, ['background','keyBackground','keyForeground','accent','actionBackground','cornerRadius','borderWidth','shadow','pattern','monospaced'], True)
# 社区列表和详情接口的可选字段。已发布的客户端拒绝未知字段，所以只有显式请求时才出现。
moderation_field = string(enum=['approved','pending','removed'],description='审核状态：仅在请求带 fields=moderation 时出现，且只出现在当前用户自己的作品上；他人的作品和匿名访问永远不带。事后审核模式下 pending 的作品已经公开，客户端只需对 removed 显示「已下架」，不显示下架原因。')
moderation_param = {'name':'fields','in':'query','schema':string(enum=['','moderation'],default=''),'description':'moderation 表示在自己的作品上接收 moderation 审核状态；其他值返回 400 invalid_fields。不带此参数时响应与以前逐字节相同。'}
# 收藏：fields=saved 时每个条目带 saved 和 saves，同样只发给显式请求的客户端。
saved_fields = {'saved':{'type':'boolean','description':'当前用户是否收藏，匿名为 false。仅在请求带 fields=saved 时出现。'},'saves':{'type':'integer','description':'收藏总数。仅在请求带 fields=saved 时出现。'}}
community_fields_param = {'name':'fields','in':'query','schema':string(enum=['','moderation','saved','moderation,saved'],default=''),'description':'逗号分隔：moderation 表示在自己的作品上接收 moderation 审核状态；saved 表示每个条目带上 saved 与 saves。其他值返回 400 invalid_fields。不带此参数时响应与以前逐字节相同。'}
community_scope_param = {'name':'scope','in':'query','schema':string(enum=['','mine','saved'],default=''),'description':'mine 只列出自己的作品（含已下架）；saved 只列出自己收藏的作品，按收藏时间倒序。两者都需要用户会话，否则 401 user_session_required；其他值返回 400 invalid_scope。'}
save_request = obj({'saved':{'type':'boolean'}},['saved'],True)
save_response = obj({'saved':{'type':'boolean'},'saves':{'type':'integer','description':'收藏总数'}},['saved','saves'])
screening_responses = {'422':{'description':'名称、描述或内容命中拦截级敏感词（blocked_content），未保存；请修改后再提交','content':{'application/json':{'schema':{'$ref':'#/components/schemas/Error'}}}},'503':{'description':'服务不可用；或敏感词检查暂时不可用（screening_unavailable，带 Retry-After，未保存，稍后重试）','headers':{'Retry-After':{'description':'screening_unavailable 时的重试等待秒数','schema':{'type':'integer'}}}}}
# 图库分类只是发布元数据，不属于 skin.toml 或键盘皮肤的 design；键盘皮肤与候选窗皮肤共用这组取值，与 internal/account 的 candidateSkinCategories 一致。
candidate_categories=['nature','guofeng','acg','cute','food','tech','minimal','other']
candidate_category=string(enum=candidate_categories,description='图库分类：nature 自然、guofeng 国风、acg 二次元、cute 可爱、food 美食、tech 科技夜色、minimal 简约、other 其他。')
category_include_param={'name':'include','in':'query','schema':string(enum=['','category'],default=''),'description':'category 表示每个条目带上 category 字段；不带时响应与引入分类之前逐字节相同。其他值返回 400 invalid_include。'}
community_skin = obj({'id':string(),'name':string(),'description':string(),'author':string(),'design':community_design,'downloads':{'type':'integer'},'rating_count':{'type':'integer'},'rating_average':{'type':'number'},'owned':{'type':'boolean'},'my_rating':{'type':'integer'},'moderation':moderation_field,'category':dict(candidate_category,description=candidate_category['description']+'仅在请求带 include=category 时出现；已发布客户端拒绝未知字段。'),**saved_fields})
for path,method,title,body,response,status in [
 ('/v1/community/skins','get','浏览用户皮肤',None,obj({'skins':{'type':'array','items':community_skin},'has_more':{'type':'boolean'}}),'200'),
 ('/v1/community/skins','post','发布用户皮肤',obj({'id':string(format='uuid'),'name':string(maxLength=32),'description':string(maxLength=280),'design':community_design,'category':dict(candidate_category,description=candidate_category['description']+'缺省（或 null）为 other，未知值或空串返回 400 invalid_category；不参与重试比较，同一内容换分类重试仍返回 200 且保留已存的分类。滚动升级期间旧版本副本会以 400 invalid_json 拒绝该键。')},['id','name','description','design'],True),obj({'id':string()}),'201'),
 ('/v1/community/skins/{id}','get','用户皮肤详情',None,community_skin,'200'),
 ('/v1/community/skins/{id}','patch','作者修改皮肤分类',obj({'category':candidate_category},['category'],True),community_skin,'200'),
 ('/v1/community/skins/{id}','delete','作者下架皮肤',None,obj({'deleted':{'type':'boolean'}}),'200'),
 ('/v1/community/skins/{id}/download','post','下载皮肤并去重计数',None,obj({'design':community_design}),'200'),
 ('/v1/community/skins/{id}/rating','put','提交或修改评分',obj({'stars':{'type':'integer','minimum':1,'maximum':5}},['stars'],True),obj({'stars':{'type':'integer'}}),'200'),
 ('/v1/community/skins/{id}/save','put','收藏或取消收藏皮肤',save_request,save_response,'200')
]:
    parameters=[]
    if '{id}' in path: parameters.append({'name':'id','in':'path','required':True,'schema':string(format='uuid')})
    elif method=='get': parameters=[{'name':'q','in':'query','schema':string(maxLength=128)},{'name':'offset','in':'query','schema':{'type':'integer','minimum':0,'maximum':100000,'default':0}},community_scope_param]
    if method=='get' and path in ('/v1/community/skins','/v1/community/skins/{id}'): parameters.append(community_fields_param)
    if method=='patch' and path=='/v1/community/skins/{id}': parameters.append(moderation_param)
    if method=='get' and path=='/v1/community/skins': parameters.append({'name':'category','in':'query','schema':string(enum=candidate_categories),'description':'只列出该图库分类的作品；未知分类返回 400 invalid_category。'})
    if response is community_skin or method=='get' and path=='/v1/community/skins': parameters.append(category_include_param)
    operation={'summary':title,'tags':['皮肤社区'],'security':[] if method=='get' else [{'userSession':[]}], 'parameters':parameters,
      'description':'仅支持数据型 Apple 键盘 v1。发布最多 50 款，重试使用相同 UUID；下载人数按账号去重。登录即可评分，不能给自己的作品评分（403 download_before_rating_or_own_skin），已下架的作品不能评分（404）。收藏请求体为 {"saved":bool}，重复提交结果相同，返回收藏状态与收藏总数；已下架的作品只有作者本人可收藏，其他人 404。列表每页 20 条，不包含照片字节。作者自己已下架的作品只对作者可见。'+('仅作者可修改（他人或不存在返回 404 skin_not_found），请求体只有 category（缺省、null 或未知值返回 400 invalid_category），返回与详情相同的作品；不改变审核状态，设为当前值同样返回 200。' if method=='patch' else '')+'详见 docs/skin-community.md。',
      'responses':{status:{'description':'成功','content':{'application/json':{'schema':response}}},**{c:{'description':m} for c,m in [('400','参数无效'),('401','需要用户登录'),('403','正在评价自己的作品'),('404','皮肤不存在或非作者'),('409','发布配额已满或 UUID 冲突'),('429','请求过多'),('503','服务不可用')]}}}
    if body: operation['requestBody']={'required':True,'content':{'application/json':{'schema':body}}}
    if path=='/v1/community/skins' and method=='post':
        operation['responses']['200']={'description':'同一发布请求的安全重试','content':{'application/json':{'schema':response}}}
        operation['responses'].update(screening_responses)
    paths.setdefault(path,{})[method]=operation
# 候选窗皮肤包：skin.toml 加 PNG/JPEG，与 /v1/skins 精选目录分表，服务端重新编码图片。
candidate_license = obj({'code':string(),'assets':string(),'source':string()},['code','assets','source'])
community_candidate_skin = obj({'id':string(format='uuid'),'package_id':string(pattern='^[a-z0-9][a-z0-9._-]{0,63}$'),'name':string(),'description':string(),'author':string(),'version':string(),'license':candidate_license,'size':{'type':'integer','description':'重新编码后的图片总字节数'},'file_count':{'type':'integer','description':'图片数量，不含 skin.toml'},'downloads':{'type':'integer'},'rating_count':{'type':'integer'},'rating_average':{'type':'number'},'owned':{'type':'boolean'},'my_rating':{'type':'integer'},'created_at':string(format='date-time'),'visibility':string(enum=['private','public'],description='同步字段：仅在列表、详情带 fields=sync，发布请求带 visibility，或 sync、PUT、PATCH 接口的响应中出现；已发布客户端拒绝未知字段。'),'updated_at':string(format='date-time',description='同步字段，替换包或切换可见性时更新。'),'request_sha256':string(pattern='^[0-9a-f]{64}$',description='同步字段，仅作者可见：上传请求原始字节的摘要。'),'category':dict(candidate_category,description=candidate_category['description']+'仅在请求带 include=category 时出现；已发布客户端拒绝未知字段。'),'moderation':moderation_field,**saved_fields})
candidate_files = {'type':'object','minProperties':1,'maxProperties':3,'additionalProperties':string(format='byte'),'description':'键为包内相对路径（仅 png/jpg/jpeg），值为标准 base64。'}
community_candidate_skin_publish = obj({'id':string(format='uuid'),'name':string(minLength=1,maxLength=32),'description':string(maxLength=280),'manifest':string(maxLength=65536,description='原样的 skin.toml 文本'),'files':candidate_files,'visibility':string(enum=['private','public'],description='缺省为 public；带上该字段即选择在响应中接收同步字段。'),'category':dict(candidate_category,description=candidate_category['description']+'缺省为 other，未知值返回 400 invalid_category；不计入 request_sha256。')},['id','name','description','manifest','files'],True)
community_candidate_skin_replace = obj({'name':string(minLength=1,maxLength=32),'description':string(maxLength=280),'manifest':string(maxLength=65536,description='原样的 skin.toml 文本，id 须与原包相同'),'files':candidate_files},['name','description','manifest','files'],True)
community_candidate_skin_sync_item = obj({'id':string(format='uuid'),'package_id':string(),'request_sha256':string(pattern='^[0-9a-f]{64}$'),'visibility':string(enum=['private','public']),'updated_at':string(format='date-time')},['id','package_id','request_sha256','visibility','updated_at'])
community_candidate_skin_package = obj({'id':string(format='uuid'),'package_id':string(),'manifest':string(),'files':candidate_files})
candidate_rules='只接受 skin.toml 加 PNG/JPEG 图片（最多 3 个文件，均须被清单引用）：单个图片不超过 1 MiB、合计不超过 2 MiB，每边 1 到 2048 像素、整包不超过 800 万像素；必须用 preview 指定一张包内图片作为预览图（重新编码后不超过 256 KiB），公开作品的 [license] 必须填写非空 assets，私有作品可省略。服务器解码后重新编码图片，去除 EXIF、XMP、ICC 等元数据。'
for path,method,title,body,response,status,description in [
 ('/v1/community/candidate-skins','get','浏览候选窗皮肤',None,obj({'skins':{'type':'array','maxItems':20,'items':community_candidate_skin},'has_more':{'type':'boolean'}}),'200','公开目录，按发布时间倒序每页 20 条，不含清单和图片字节。scope=mine 只列出自己的作品，scope=saved 只列出自己收藏的作品（按收藏时间倒序），都需要用户会话；只有 scope 非空且 fields 含 sync 时才包含自己的私有作品，别人的私有作品永远不出现。category 按图库分类筛选，未知分类返回 400 invalid_category。'),
 ('/v1/community/candidate-skins','post','发布候选窗皮肤包',community_candidate_skin_publish,community_candidate_skin,'201','请求最多 3,200,000 字节（高于其他 JSON 接口的 64 KiB）。'+candidate_rules+'id 为客户端 UUID，同一请求重试返回 200；每个账号最多 100 款，其中公开最多 20 款。公开发布每小时最多 10 次，私有创建与替换共用每小时 60 次。'),
 ('/v1/community/candidate-skins/sync','get','同步自己的候选窗皮肤库',None,obj({'skins':{'type':'array','maxItems':100,'items':community_candidate_skin_sync_item}}),'200','返回自己的全部作品（含私有），按 updated_at 倒序，不分页。'),
 ('/v1/community/candidate-skins/{id}','get','候选窗皮肤详情',None,community_candidate_skin,'200','公开详情；登录时额外返回自己的评分与是否为作者。私有作品仅作者带 fields=sync 可见，其他情况返回 404。'),
 ('/v1/community/candidate-skins/{id}','put','作者替换候选窗皮肤包',community_candidate_skin_replace,community_candidate_skin,'200','仅作者可替换，校验与限制同发布；清单 id 须与原包相同，否则 409。保留 id、可见性、发布时间、下载与评分，重新计算 request_sha256 并更新 updated_at；与已存内容相同时不写入。计入每小时 60 次的私有额度。'),
 ('/v1/community/candidate-skins/{id}','patch','作者修改候选窗皮肤的可见性或分类',dict(obj({'visibility':string(enum=['private','public']),'category':candidate_category},None,True),minProperties=1),community_candidate_skin,'200','仅作者可修改，visibility 与 category 至少带一个（都缺省返回 400 invalid_visibility）。转为公开须声明 assets 许可、占用公开配额，并计入每小时 10 次的发布额度；修改分类计入每小时 60 次的私有额度，不更新 updated_at，也不重新进入审核。'),
 ('/v1/community/candidate-skins/{id}','delete','作者下架候选窗皮肤',None,obj({'deleted':{'type':'boolean'}}),'200','仅作者可下架，连带删除图片、下载和评分记录。'),
 ('/v1/community/candidate-skins/{id}/preview','get','候选窗皮肤预览图',None,obj({'path':string(),'content_type':string(enum=['image/png','image/jpeg']),'data':string(format='byte')}),'200','返回重新编码后的预览图，data 为标准 base64；私有作品仅作者可取。'),
 ('/v1/community/candidate-skins/{id}/download','post','下载候选窗皮肤包并去重计数',None,community_candidate_skin_package,'200','返回原样清单和重新编码后的图片；下载人数按账号去重。私有作品仅作者可下载。'),
 ('/v1/community/candidate-skins/{id}/rating','put','为候选窗皮肤评分',obj({'stars':{'type':'integer','minimum':1,'maximum':5}},['stars'],True),obj({'stars':{'type':'integer'}}),'200','登录即可评分，不需要先下载；不能给自己的作品评分（403 download_before_rating_or_own_skin），重复提交更新同一条评分；不存在、私有或已下架的作品返回 404。'),
 ('/v1/community/candidate-skins/{id}/save','put','收藏或取消收藏候选窗皮肤',save_request,save_response,'200','请求体为 {"saved":bool}，重复提交结果相同，返回收藏状态与收藏总数。不存在、已下架（作者本人除外）或别人的私有作品收藏时返回 404 skin_not_found；取消收藏不看作品状态，总是返回 200。'),
]:
    parameters=[]
    fields={'name':'fields','in':'query','schema':string(enum=['','sync','moderation','saved','sync,moderation','sync,saved','moderation,saved','sync,moderation,saved'],default=''),'description':'逗号分隔：sync 表示在响应中接收同步字段并可看到自己的私有作品；moderation 表示在自己的作品上接收审核状态；saved 表示每个条目带上 saved 与 saves。其他值返回 400 invalid_fields。'}
    if '{id}' in path: parameters.append({'name':'id','in':'path','required':True,'schema':string(format='uuid')})
    elif method=='get' and path=='/v1/community/candidate-skins': parameters=[{'name':'q','in':'query','schema':string(maxLength=128)},{'name':'offset','in':'query','schema':{'type':'integer','minimum':0,'maximum':100000,'default':0}},community_scope_param,fields]
    if path=='/v1/community/candidate-skins/{id}' and method=='get': parameters.append(fields)
    if method=='get' and path=='/v1/community/candidate-skins': parameters.append({'name':'category','in':'query','schema':string(enum=candidate_categories),'description':'只列出该图库分类的作品。'})
    if response is community_candidate_skin or method=='get' and path=='/v1/community/candidate-skins': parameters.append(category_include_param)
    operation={'summary':title,'tags':['皮肤社区'],'security':[] if method=='get' and not path.endswith('/sync') else [{'userSession':[]}],'parameters':parameters,
      'description':description+' 详见 docs/skin-community.md。',
      'responses':{status:{'description':'成功','content':{'application/json':{'schema':response}}},**{c:{'description':m} for c,m in [('400','参数、清单或图片无效'),('401','需要用户登录'),('403','正在评价自己的作品'),('404','皮肤不存在、已下架、为他人的私有作品或非作者'),('409','配额已满、UUID 冲突或替换包 id 不符'),('415','需要 application/json'),('429','请求过多'),('503','服务不可用或图片处理繁忙')]}}}
    if body: operation['requestBody']={'required':True,'content':{'application/json':{'schema':body}}}
    if path=='/v1/community/candidate-skins' and method=='post': operation['responses']['200']={'description':'同一发布请求的安全重试','content':{'application/json':{'schema':response}}}
    if (path,method) in (('/v1/community/candidate-skins','post'),('/v1/community/candidate-skins/{id}','put')): operation['responses'].update(screening_responses)
    paths.setdefault(path,{})[method]=operation
# 插件社区：plugin.toml 加音频或数据文件与说明文本的 zip 包，服务端只校验、存储和分发原始字节，从不执行。
plugin_kinds = ['sound','music','command_table','effect','helpcode','symbol_set','phrase_table','wordbook']
plugin_kind = string(enum=plugin_kinds)
plugin_kinds_param = {'name':'kinds','in':'query','schema':string(),'description':'逗号分隔的、客户端能安装的类型。sound、music、command_table、effect 总是返回；其余类型只在这里声明（或由 kind 指定）后才返回，详情未声明时为 404 plugin_not_found。不认识的名字忽略，不报错。'}
plugin_id = string(pattern='^[a-z0-9][a-z0-9._-]{0,63}$')
community_plugin = obj({'id':string(format='uuid'),'kind':plugin_kind,'plugin_id':plugin_id,'name':string(),'description':string(),'author':string(),'version':string(),'license':string(),'size':{'type':'integer','description':'zip 包字节数'},'sha256':string(pattern='^[0-9a-f]{64}$',description='zip 包的 SHA-256'),'downloads':{'type':'integer'},'rating_count':{'type':'integer'},'rating_average':{'type':'number'},'owned':{'type':'boolean'},'my_rating':{'type':'integer'},'created_at':string(format='date-time'),'moderation':moderation_field,**saved_fields})
community_plugin_publish = obj({'id':string(format='uuid'),'name':string(minLength=1,maxLength=32),'description':string(maxLength=280),'kind':plugin_kind,'plugin_id':plugin_id,'version':string(minLength=1,maxLength=32),'archive':string(format='byte',description='zip 包的标准 base64，解码后不超过 8 MiB')},['id','name','description','kind','plugin_id','version','archive'],True)
community_plugin_package = obj({'id':string(format='uuid'),'kind':plugin_kind,'plugin_id':plugin_id,'version':string(),'size':{'type':'integer'},'sha256':string(pattern='^[0-9a-f]{64}$'),'archive':string(format='byte')})
plugin_rules='zip 包不超过 8 MiB、最多 64 个成员和 16 个文件，解压总量不超过 24 MiB 且不超过包体 100 倍加 1 MiB；拒绝绝对路径、..、反斜杠、符号链接、加密成员和嵌套压缩包。包内恰好一个 plugin.toml（schema_version = 1，严格解析，未知键拒绝），kind 与 plugin_id、version 须与请求一致，permissions 必须为空；引用的音频须存在且为 .wav/.ogg 并匹配文件头（sound 采样只能是 .wav），effect、phrase_table、symbol_set 只含清单里的参数或数据、不带任何音频；helpcode 点名一个不超过 1 MiB 的 .txt 辅助码表，wordbook 点名一个不超过 4 MiB 的 .tsv 单词表，内容按客户端的严格语法校验；其余文件只能是 .txt/.md 说明。'
for path,method,title,body,response,status,description in [
 ('/v1/community/plugins','get','浏览社区插件',None,obj({'plugins':{'type':'array','maxItems':20,'items':community_plugin},'has_more':{'type':'boolean'}}),'200','按发布时间倒序每页 20 条，可按 kind 过滤、按名称搜索，不含 zip 包字节。只返回旧客户端认识的类型和 kinds 声明的类型。scope=mine 只列出自己的作品（含已下架），scope=saved 只列出自己收藏的作品（按收藏时间倒序），都需要用户会话。'),
 ('/v1/community/plugins','post','发布插件包',community_plugin_publish,community_plugin,'201','请求最多 11,300,000 字节。'+plugin_rules+'id 为客户端 UUID，同一请求重试返回 200；每个账号最多 20 个插件、包体合计不超过 32 MiB，每小时最多发布 10 次；同一账号的发布逐个处理，同一进程最多同时接收 4 个发布，排队超时返回 503 plugin_busy。'),
 ('/v1/community/plugins/{id}','get','插件详情',None,community_plugin,'200','公开详情；登录时额外返回自己的评分与是否为作者。类型不在旧客户端认识的范围内且未在 kinds 中声明时返回 404。'),
 ('/v1/community/plugins/{id}','delete','作者下架插件',None,obj({'deleted':{'type':'boolean'}}),'200','仅作者可下架，连带删除下载和评分记录。'),
 ('/v1/community/plugins/{id}/download','post','下载插件包并去重计数',None,community_plugin_package,'200','返回原样 zip 包（标准 base64）与 SHA-256，客户端安装前应校验摘要；下载人数按账号去重。每个账号每小时最多 60 次下载，同一进程最多同时发送 8 个包，排队超时返回 503 plugin_busy。'),
 ('/v1/community/plugins/{id}/rating','put','为插件评分',obj({'stars':{'type':'integer','minimum':1,'maximum':5}},['stars'],True),obj({'stars':{'type':'integer'}}),'200','登录即可评分，不需要先下载；不能给自己的作品评分（403 download_before_rating_or_own_plugin），重复提交更新同一条评分；不存在或已下架的插件返回 404。'),
 ('/v1/community/plugins/{id}/save','put','收藏或取消收藏插件',save_request,save_response,'200','请求体为 {"saved":bool}，重复提交结果相同，返回收藏状态与收藏总数。不存在或已下架（作者本人除外）的插件收藏时返回 404 plugin_not_found；取消收藏不看作品状态，总是返回 200。'),
]:
    parameters=[]
    if '{id}' in path: parameters.append({'name':'id','in':'path','required':True,'schema':string(format='uuid')})
    elif method=='get': parameters=[{'name':'q','in':'query','schema':string(maxLength=128)},{'name':'kind','in':'query','schema':string(enum=['',*plugin_kinds],default='')},{'name':'offset','in':'query','schema':{'type':'integer','minimum':0,'maximum':100000,'default':0}},community_scope_param]
    if method=='get' and path in ('/v1/community/plugins','/v1/community/plugins/{id}'): parameters += [community_fields_param,plugin_kinds_param]
    operation={'summary':title,'tags':['插件社区'],'security':[] if method=='get' else [{'userSession':[]}],'parameters':parameters,
      'description':description+' 详见 docs/plugin-community.md。',
      'responses':{status:{'description':'成功','content':{'application/json':{'schema':response}}},**{c:{'description':m} for c,m in [('400','参数、清单或 zip 包无效'),('401','需要用户登录'),('403','正在评价自己的作品'),('404','插件不存在、已下架或非作者'),('409','配额已满或 UUID 冲突'),('415','需要 application/json'),('429','请求过多'),('503','服务不可用或校验繁忙')]}}}
    if body: operation['requestBody']={'required':True,'content':{'application/json':{'schema':body}}}
    if path=='/v1/community/plugins' and method=='post':
        operation['responses']['200']={'description':'同一发布请求的安全重试','content':{'application/json':{'schema':response}}}
        operation['responses'].update(screening_responses)
    paths.setdefault(path,{})[method]=operation

shared_word = obj({'kind':string(enum=['pinyin','wubi','english','quick']),'code':string(maxLength=512),'word':string(maxLength=2048),'weight':{'type':'integer','minimum':0}},['kind','code','word','weight'],True)
shared_phrase = obj({'text':string(minLength=1,maxLength=2000,description='1–2000 个 UTF-16 单元，可以换行，不含其他控制字符，同一包内不重复。'),'group':string(maxLength=32,description='分组名，最多 32 个 UTF-16 单元，空字符串表示未分组。')},['text','group'],True)
resource_content = obj({'entries':{'type':'array','minItems':1,'maxItems':128,'items':shared_word},'prompt':string(minLength=1,maxLength=2000),'phrases':{'type':'array','minItems':1,'maxItems':200,'items':shared_phrase,'description':'仅 kind=phrase：无编码常用语。'}},[],True)
resource = obj({'id':string(format='uuid'),'kind':string(enum=['dictionary','reply','phrase']),'name':string(),'description':string(),'author':string(),'content':resource_content,'revision':{'type':'integer'},'saves':{'type':'integer'},'saved':{'type':'boolean'},'owned':{'type':'boolean'},'rating_count':{'type':'integer'},'rating_average':{'type':'number'},'my_rating':{'type':'integer'},'moderation':moderation_field})
for path,method,title,body,response in [
 ('/v1/community/resources','get','浏览词库、回复模板和短语包',None,obj({'items':{'type':'array','items':resource},'has_more':{'type':'boolean'}})),
 ('/v1/community/resources','post','发布或更新词库、回复模板与短语包',obj({'id':string(format='uuid'),'kind':string(enum=['dictionary','reply','phrase']),'name':string(minLength=1,maxLength=32),'description':string(maxLength=280),'content':resource_content,'revision':{'type':'integer','minimum':0}},['id','kind','name','description','content','revision'],True),obj({'id':string(),'revision':{'type':'integer'}})),
 ('/v1/community/resources/{id}','get','作品内容与版本',None,resource),
 ('/v1/community/resources/{id}','delete','作者下架作品',None,obj({'deleted':{'type':'boolean'}})),
 ('/v1/community/resources/{id}/apply','post','原子导入词包到个人词库（同词条更新权重，保留其他词条；短语包返回 400 unsupported_kind，由客户端在本地合并）',obj({'resource_revision':{'type':'integer','minimum':1},'dictionary_revision':{'type':'integer','minimum':0}},['resource_revision','dictionary_revision'],True),obj({'revision':{'type':'integer'},'imported':{'type':'integer'},'resource_revision':{'type':'integer'}})),
 ('/v1/community/resources/{id}/save','put','收藏或取消收藏',obj({'saved':{'type':'boolean'}},['saved'],True),obj({'saved':{'type':'boolean'}})),
 ('/v1/community/resources/{id}/rating','put','评分',obj({'stars':{'type':'integer','minimum':1,'maximum':5}},['stars'],True),obj({'stars':{'type':'integer'}}))
]:
    parameters=[]
    if '{id}' in path: parameters.append({'name':'id','in':'path','required':True,'schema':string(format='uuid')})
    elif method=='get': parameters=[{'name':'kind','in':'query','required':True,'schema':string(enum=['dictionary','reply','phrase'])},{'name':'scope','in':'query','schema':string(enum=['','saved','mine'])},{'name':'q','in':'query','schema':string(maxLength=128)},{'name':'offset','in':'query','schema':{'type':'integer','minimum':0,'maximum':1000000}}]
    if method=='get' and path in ('/v1/community/resources','/v1/community/resources/{id}'): parameters.append(moderation_param)
    operation={'summary':title,'tags':['创作社区'],'security':[] if method=='get' else [{'userSession':[]}],'parameters':parameters,
      'description':'发现和详情公开；saved/mine 范围需要用户会话。词库仅携带显式选定的 1–128 条记录，由 Engine 校验；回复仅携带提示词（不含密钥）；短语包仅携带 1–200 条无编码常用语（content.phrases），客户端本地安装进常用语。列表只返回请求的 kind，旧客户端不会收到短语包。每账号最多 50 份。新建 revision=0，更新携带当前 revision，冲突返回 409；相同内容重试不增加版本。收藏按账号去重。登录即可评分，不需要先收藏；不能给自己的作品评分（403 save_before_rating_or_own_resource），不存在或已下架的作品返回 404 resource_not_found。查看版本不会自动覆盖个人词库。',
      'responses':{'200':{'description':'成功','content':{'application/json':{'schema':response}}},**{code:{'description':message} for code,message in [('400','内容无效'),('401','需要登录'),('403','正在评价自己的作品'),('404','作品不存在或非作者'),('409','版本冲突或达到上限'),('429','请求过多'),('503','服务不可用')]}}}
    if body: operation['requestBody']={'required':True,'content':{'application/json':{'schema':body}}}
    if method=='post': operation['responses']['201']=operation['responses']['200']
    if path=='/v1/community/resources' and method=='post': operation['responses'].update(screening_responses)
    paths.setdefault(path,{})[method]=operation
count=lambda description:{'type':'integer','minimum':0,'description':description}
community_stats=obj({'skins':count('已发布的用户皮肤数'),'skin_downloads':count('皮肤下载人次（按账号和皮肤去重）'),'dictionaries':count('已发布的共享词库数'),'replies':count('已发布的回复模板数'),'resource_saves':count('词库与回复模板收藏人次（按账号和作品去重）'),'generated_at':string(format='date-time',description='数据库统计时间（UTC）')},['skins','skin_downloads','dictionaries','replies','resource_saves','generated_at'])
paths['/v1/community/stats']={'get':{'summary':'社区内容统计','tags':['创作社区'],'security':[],
    'description':'公开只读，供官网展示。只统计社区作品与去重后的下载、收藏人次，不含用户数和安装包上报；注销账号的作品与互动随之移除。响应禁用缓存，调用方自行缓存。',
    'responses':{'200':{'description':'成功','content':{'application/json':{'schema':community_stats}}},'429':{'description':'请求过多','headers':{'Retry-After':{'description':'重试等待秒数','schema':{'type':'integer'}}}},'503':{'description':'用户体系未启用或数据库不可用'}}}}

site_mirrors=obj({'lanzou_url':string(maxLength=512,description='蓝奏云盘分享链接（https 绝对地址）；未设置或已清空时为空字符串。'),'updated_at':string(description='链接最近更新时间（UTC，RFC 3339）；lanzou_url 为空时为空字符串。')},['lanzou_url','updated_at'])
paths['/v1/site/download-mirrors']={'get':{'summary':'官网下载镜像链接','tags':['官网'],'security':[],
    'description':'公开只读，供官网下载页展示 Windows 安装包的蓝奏云盘链接，由管理员在后台「站点设置」中修改。与 /v1/notices 共用按 IP 每分钟 1200 次的限流（不占用登录等用户接口的 120 次）；成功响应带 Cache-Control: public, max-age=60 和 Vary: Origin，允许官网和边缘缓存短时缓存。用户体系未启用时返回 503。',
    'responses':{'200':{'description':'成功','content':{'application/json':{'schema':site_mirrors}}},'429':{'description':'请求过多','headers':{'Retry-After':{'description':'重试等待秒数','schema':{'type':'integer'}}}},'503':{'description':'用户体系未启用或数据库不可用'}}}}

# Anonymous website word form (msime-web#213). Errors here use a plain-string error plus code, which the website form reads.
word_error=obj({'error':string(description='可直接展示给用户的中文说明。'),'code':string()},['error','code'])
word_rejected=obj({'error':string(),'code':string(enum=['invalid_entries']),'rejected':{'type':'array','items':obj({'index':{'type':'integer','minimum':0,'description':'entries 中的下标。'},'code':string(enum=['word_required','invalid_word','word_too_long','pinyin_required','invalid_pinyin','invalid_syllable','syllable_count_mismatch','display_required','invalid_display','display_too_long','source_required','invalid_source','source_too_long','gloss_required','invalid_gloss','gloss_too_long','duplicate_entry','already_listed','blocked_word'],description='words 类型用 word_*、pinyin_*、*_syllable*；english 类型用 word_*、display_*；translations 类型用 source_*、gloss_*；duplicate_entry 与 already_listed 各类型通用。 blocked_word 表示该词条命中管理后台配置的屏蔽词，各类型通用；只给出类别，不会返回具体屏蔽词。'),'reason':string(description='可直接展示在该行旁的中文说明。')},['index','code','reason'])}},['error','code'])
def word_response(description, schema=word_error, retry=False):
    r={'description':description,'content':{'application/json':{'schema':schema}}}
    if retry: r['headers']={'Retry-After':{'description':'重试等待秒数','schema':{'type':'integer'}}}
    return r
paths['/v1/community/word-submissions']={
 'get':{'summary':'查询官网词条提交是否开放','tags':['词条提交'],'security':[],
  'description':'匿名接口，无需令牌。未配置时 enabled=false、site_key 为空字符串。site_key 是 Cloudflare Turnstile 站点密钥（公开值）。',
  'responses':{'200':{'description':'成功','content':{'application/json':{'schema':obj({'enabled':{'type':'boolean'},'site_key':string()},['enabled','site_key'])}}}}},
 'post':{'summary':'匿名提交词条、英文单词或翻译到 msime-dictionary 滚动 Pull Request','tags':['词条提交'],'security':[],
  'description':'仅接受 allowed_origins 中网站发出的浏览器请求（必须携带 Origin）。请求体最多 16 KiB。服务端依次校验词条、Cloudflare Turnstile（action 为 words，hostname 属于 allowed_origins）以及按客户端地址的 PostgreSQL 限流（每 10 分钟 3 次、每天 20 次，只计通过人机验证的请求），然后用仅限 msime-dictionary、仅有 contents:write 与 pull_requests:write 的 GitHub App 安装令牌按 kind 追加：words 把 `词语<TAB>拼音<TAB>权重` 追加到 custom/words.txt（权重取基础词库同音节数词条的权重中位数，8 及以上音节合并计算，并限制在 words.txt 现有权重范围内；Engine 无法计算时用 5000）；english 把 `单词<TAB>显示词形<TAB>1` 追加到 custom/english.txt；translations 把 `原词<TAB>译文` 追加到 custom/translations.txt（同一原词的后一行覆盖前一行，所以允许修正已有翻译，只拒绝完全相同的一对）。三类共用一个滚动 Pull Request，标题按分支相对主分支新增的行数汇总，例如 feat(custom): add 3 words, 1 English word and 2 translations。已有开启的 community-words/* Pull Request 时追加提交，否则从主分支新建 community-words/<UTC yyyymmdd-hhmmss> 分支并开 Pull Request。写入使用文件 blob SHA 做乐观并发，冲突返回 409，服务端从不自动重试 GitHub 写入。备注会公开写入提交说明，@、#、GH- 与 :// 会插入零宽空格。服务不保存访客信息，不记录词条、备注或令牌。 写入前会用管理后台的屏蔽词库检查每个词条的各列和备注：命中屏蔽级的词条以 blocked_word 拒绝，备注命中则整次提交返回 400 blocked_word；命中待审级只在提交说明里标出词条位置和类别，供审核参考。',
  'requestBody':{'required':True,'content':{'application/json':{'schema':obj({
    'kind':string(enum=['words','english','translations'],default='words',description='提交类型，决定 entries 每项的字段；省略时为 words。其他值返回 400 invalid_kind。'),
    'entries':{'type':'array','minItems':1,'maxItems':20,'description':'1–20 项，字段由 kind 决定，多余字段返回 invalid_json。','items':{'oneOf':[
      obj({'word':string(minLength=1,description='1–16 个 CJK 统一表意文字（含各扩展区）或〇，不含字母、数字、标点、空白和控制字符。',example='未来可期'),'pinyin':string(maxLength=200,pattern="^[a-z]+('[a-z]+)*$",description="小写全拼，音节用 ' 连接，音节数等于字数；ü 写作 v，lüe/nüe 写作 lve/nve。音节表与官网表单一致（402 个）。",example="wei'lai'ke'qi")},['word','pinyin'],True)|{'title':'words'},
      obj({'word':string(minLength=1,maxLength=64,pattern='^[a-z]+$',description='输入时键入的编码，只能是小写 ASCII 字母。',example='github'),'display':string(minLength=1,description='候选中显示的词形，首尾空白会被去掉；最多 64 个字符，不含制表符、换行、零宽等控制或格式字符。',example='GitHub')},['word','display'],True)|{'title':'english'},
      obj({'source':string(minLength=1,description='原词（中文或英文），首尾空白会被去掉；最多 64 个字符，不能以 # 开头，不含制表符、换行等控制或格式字符。',example='苹果'),'gloss':string(minLength=1,description='译文，首尾空白会被去掉；最多 200 个字符，不含制表符、换行等控制或格式字符。',example='apple')},['source','gloss'],True)|{'title':'translations'}]}},
    'note':string(maxLength=500,description='可选备注，最多 500 个字符；换行和控制字符会被压成空格。'),
    'token':string(minLength=1,maxLength=2048,description='Turnstile 令牌，一次有效。')},['entries','token'],True)}}},
  'responses':{
    '201':{'description':'已写入 Pull Request','content':{'application/json':{'schema':obj({'pull_request_url':string(format='uri',example='https://github.com/metasequoiaime/msime-dictionary/pull/12')},['pull_request_url'])}}},
    '400':word_response('请求无效（code 为 invalid_json、invalid_kind、invalid_entry_count、invalid_note、token_required）；备注命中屏蔽词时 code 为 blocked_word，整次提交不写入；词条问题在 rejected 中逐条给出（包括词库、英文词库或分支文件中已有的 already_listed，以及命中屏蔽词的 blocked_word）',{'oneOf':[word_rejected,word_error]}),
    '403':word_response('来源不是配置的网站，或 Turnstile 验证失败'),
    '409':word_response('目标文件被同时修改（或同一秒内新建了同名分支），词条未写入；由用户重新验证后手动重试'),
    '413':word_response('请求体超过 16 KiB'),
    '415':word_response('需要 application/json'),
    '429':word_response('提交过于频繁',retry=True),
    '502':word_response('GitHub 写入结果未知，词条可能已写入；先查看 pulls_url 再决定是否重试',obj({'error':string(),'code':string(enum=['outcome_unknown']),'uncertain':{'type':'boolean','enum':[True]},'pulls_url':string(format='uri')},['error','code','uncertain'])),
    '503':word_response('功能未开启或配置无效、Turnstile/数据库/GitHub 暂不可用、屏蔽词检查暂不可用（screening_unavailable），或其他副本对同一仓库的写入 15 秒内未完成（server_busy）；此时尚未写入任何词条',retry=True)}}}

paths['/v1/telemetry/events']={'post': {'summary': '上报崩溃、匿名设备活跃与会话事件，以及官网镜像下载', 'tags': ['统计'], 'security': [], 'description': '无需鉴权：不需要设备令牌或用户会话，带了 Authorization 也会被忽略（不校验），所以未登录的客户端和官网都能上报。需要用户数据库及最新迁移。按客户端地址限流：每个地址每分钟 60 次（独立额度，不占用登录和社区接口的 120 次），crash 事件每个地址每天另限 20 次（超出返回 429，Retry-After 3600）；部署在反向代理后由顶层 client_ip_header 指定客户端地址头。请求体最多 32 KiB，只接受下列字段，未知字段返回 400 invalid_json。事件 ID 全局唯一（推荐 UUID v4），重试复用 ID，重复事件返回 202 且不重复计数。时间以服务端接收时间为准。kind：active 为一台安装当天活跃，每个安装每个 UTC 日最多一次，id 建议用 active-<install_id>-<yyyymmdd>；session 为一次正常结束的会话（输入法宿主进程的一次生命周期，iOS 键盘为一次显示），session_crash 为一次以崩溃结束的会话，只有本地存在该会话的崩溃记录时才算，进程被系统回收、注销或关机留下的会话标记不算崩溃；crash 为一次崩溃，必须有 message，可带 stack，服务端按 message 与首个非系统栈帧归入崩溃分组；download 只由官网在用户点击国内镜像链接时上报（channel=cn-mirror），客户端不发 download，GitHub Release 下载取自服务端的 Release 快照。只有 crash 可以携带 message/stack，其他 kind 携带即为 400 invalid_event。crash 的 message 与 stack 中 CRLF 和单独的 CR 统一转为 LF；stack 超过 16000 个字符时在最后一个完整行处截断后照常接收，但请求体仍受 32 KiB 限制，客户端应把 stack 的 UTF-8 字节数控制在约 12 KB 以内。install_id 必须是安装时随机生成、保存在本机的匿名标识，不得来自硬件、账号或用户数据，每个事件都应带上（崩溃页的影响设备数按它去重）。响应：202 已接收；400 丢弃该事件，不要重试；429（遵守 Retry-After）、5xx 和网络错误保留事件，稍后用同一 ID 重试。不要上传输入内容、密码或个人信息。', 'requestBody': {'required': True, 'content': {'application/json': {'schema': {'type': 'object', 'properties': {'id': {'type': 'string', 'minLength': 16, 'maxLength': 128}, 'kind': {'type': 'string', 'enum': ['download', 'crash', 'active', 'session', 'session_crash'], 'description': '事件类型，见接口说明。'}, 'platform': {'type': 'string', 'minLength': 1, 'maxLength': 32, 'description': '规范平台 ID：windows、macos、linux、android、ios、harmony。后台统计时也会归一 win、mac、darwin、ipados、harmonyos、ohos 等别名。'}, 'version': {'type': 'string', 'minLength': 1, 'maxLength': 64, 'description': '真实的应用版本号。'}, 'message': {'type': 'string', 'maxLength': 1000, 'description': '仅 crash：异常或信号摘要，必填，首行应有意义，可含换行，最多 1000 个 Unicode 标量。'}, 'stack': {'type': 'string', 'description': '仅 crash：调用栈，可含换行；帧写作模块+偏移或符号，模块路径只保留文件名。超过 16000 个 Unicode 标量时服务端在行边界截断。'}, 'artifact': {'type': 'string', 'minLength': 1, 'maxLength': 64, 'description': '可选：下载的安装包文件名或产物名，单行，不含控制字符。', 'example': 'msime-windows-x64-setup.exe'}, 'channel': {'type': 'string', 'pattern': '^[a-z0-9][a-z0-9_-]{0,31}$', 'description': '可选：分发渠道键。后台识别 cn-mirror、website、github、app-store、testflight、appgallery、google-play，其他值按原样显示。', 'example': 'cn-mirror'}, 'install_id': {'type': 'string', 'pattern': '^[A-Za-z0-9_-]{16,64}$', 'description': '匿名安装标识，16–64 个 [A-Za-z0-9_-]，安装时随机生成；kind=active 时必填，其他 kind 也应带上。'}}, 'required': ['id', 'kind', 'platform', 'version'], 'additionalProperties': False}}}}, 'responses': {'202': {'description': '已接收（含重复事件）'}, '400': {'description': '事件无效，丢弃不要重试：invalid_event（字段越界、kind 未知、非 crash 事件携带 message/stack、crash 缺少 message 或 active 缺少 install_id）或 invalid_json（JSON 畸形、含未知字段或超过 32 KiB）'}, '403': {'description': '浏览器来源不在 allowed_origins 中（origin_denied）'}, '415': {'description': '需要 JSON'}, '429': {'description': '请求过多，保留事件并按 Retry-After 稍后重试', 'headers': {'Retry-After': {'description': '重试等待秒数', 'schema': {'type': 'integer'}}}}, '503': {'description': '用户体系未启用或数据库不可用，稍后重试'}}}}

# Admin console public endpoints: community reports and published notices.
paths['/v1/community/reports']={'post': {'summary': '举报社区内容', 'tags': ['创作社区'], 'security': [{'userSession': []}], 'description': '登录用户举报一项社区内容，由管理后台复核。kind 取 skins、candidate-skins、plugins、dictionaries、replies、phrases（短语包）；只能举报自己可见的内容（已下架内容与私有候选皮肤返回 404）。同一账号重复举报同一内容不会新增记录，返回 200；每个账号每小时最多 30 次。', 'requestBody': {'required': True, 'content': {'application/json': {'schema': {'type': 'object', 'properties': {'kind': {'type': 'string', 'enum': ['skins', 'candidate-skins', 'plugins', 'dictionaries', 'replies', 'phrases']}, 'item_id': {'type': 'string', 'minLength': 1, 'maxLength': 128}, 'reason': {'type': 'string', 'minLength': 1, 'maxLength': 64, 'description': '举报原因，不含控制字符'}, 'detail': {'type': 'string', 'maxLength': 1000, 'description': '补充说明，可含换行'}}, 'required': ['kind', 'item_id', 'reason'], 'additionalProperties': False}}}}, 'responses': {'200': {'description': '已举报过，未新增记录', 'content': {'application/json': {'schema': {'type': 'object', 'properties': {'reported': {'type': 'boolean'}}}}}}, '201': {'description': '已记录举报', 'content': {'application/json': {'schema': {'type': 'object', 'properties': {'reported': {'type': 'boolean'}}}}}}, '400': {'description': '参数无效'}, '401': {'description': '需要用户登录'}, '404': {'description': '内容不存在或不可见'}, '429': {'description': '请求过多'}, '503': {'description': '服务不可用'}}}}
paths['/v1/notices']={'get': {'summary': '公告列表', 'tags': ['公告'], 'security': [], 'description': '公开只读，无需鉴权，供 App 与官网展示管理后台已发布的公告，按发布时间倒序最多 20 条；草稿和已归档的公告不会出现。响应允许缓存 60 秒（Cache-Control: public, max-age=60，并带 Vary: Origin），发布或归档最多 60 秒后可见。与 /v1/site/download-mirrors 共用按 IP 每分钟 1200 次的限流，不占用登录等用户接口的 120 次。客户端不要比 max-age 更频繁地请求：在设置窗口或 App 首页打开时拉取，不要由输入法进程在后台轮询。', 'parameters': [{'name': 'platform', 'in': 'query', 'required': False, 'description': '只返回投放到该平台（或全部平台）的公告。也接受别名（不区分大小写）：win、mac、darwin、ipados、harmonyos、ohos，按对应的规范平台匹配；其他值返回 400。', 'schema': {'type': 'string', 'enum': ['windows', 'macos', 'linux', 'android', 'ios', 'harmony']}}, {'name': 'channel', 'in': 'query', 'required': False, 'description': '只返回包含该渠道的公告', 'schema': {'type': 'string', 'enum': ['site', 'app', 'telegram']}}], 'responses': {'200': {'description': '成功', 'content': {'application/json': {'schema': {'type': 'object', 'properties': {'items': {'type': 'array', 'items': {'type': 'object', 'properties': {'id': {'type': 'string', 'description': '公告 ID'}, 'title': {'type': 'string', 'maxLength': 200, 'description': '标题'}, 'body': {'type': 'string', 'maxLength': 20000, 'description': '正文，简单 Markdown。客户端渲染时必须禁用原始 HTML，链接在外部浏览器打开。'}, 'targets': {'type': 'array', 'items': {'type': 'string', 'enum': ['all', 'windows', 'macos', 'linux', 'android', 'ios', 'harmony']}, 'description': '投放平台；all 表示全部平台'}, 'channels': {'type': 'array', 'items': {'type': 'string', 'enum': ['site', 'app', 'telegram']}, 'description': '展示渠道：site 官网横幅，app App 内通知，telegram 已推送到 Telegram'}, 'published_at': {'type': 'string', 'format': 'date-time', 'description': '发布时间（UTC）'}}, 'required': ['id', 'title', 'body', 'targets', 'channels', 'published_at']}}}, 'required': ['items']}}}}, '400': {'description': 'platform 或 channel 不在取值范围内（invalid_platform / invalid_channel）', 'content': {'application/json': {'schema': {'$ref': '#/components/schemas/Error'}}}}, '429': {'description': '请求过多', 'headers': {'Retry-After': {'description': '重试等待秒数', 'schema': {'type': 'integer'}}}}, '503': {'description': '用户体系未启用或数据库不可用'}}}}

paths['/v1/skins/generate']={'post':{'summary':'生成原创皮肤插画背景','tags':['皮肤'],'description':'只发送风格描述，模型由服务端配置。返回一张 PNG/JPEG，尺寸不超过 2048×2048，图像最多 8 MiB；不保存或自动公开。','requestBody':{'required':True,'content':{'application/json':{'schema':obj({'prompt':string(minLength=1,maxLength=1200)},['prompt'],True)}}},'responses':{'200':{'description':'生成成功','content':{'application/json':{'schema':obj({'b64_json':string(format='byte'),'mime_type':string(enum=['image/png','image/jpeg']),'width':{'type':'integer'},'height':{'type':'integer'}})}}},'400':{'description':'描述无效'},'401':{'description':'需要认证'},'502':{'description':'生成结果无效'},'503':{'description':'未配置或繁忙'}}}}

artwork_schema=paths['/v1/skins/generate']['post']['responses']['200']['content']['application/json']['schema']
job_id=string(pattern='^[0-9a-f]{48}$')
job_status=obj({'id':job_id,'state':string(enum=['running','succeeded','failed']),'artwork':dict(artwork_schema,nullable=True)},['id','state'])
paths['/v1/skins/jobs']={'post':{'summary':'提交原创皮肤插画任务','tags':['皮肤'],'description':'立即返回任务 ID，不等待生图。任务仅当前认证主体可访问；每主体最多 3 个任务，全局最多 min(8,max_concurrent) 个。草稿保留 10 分钟，不是持久化皮肤。启用数据库时任务存于共享表，任何副本都能查询和删除，上限按整个部署计算；未启用数据库时任务只在接受它的进程内，服务重启会失效。领取结果后应 DELETE 释放。不要自动重试提交，避免重复生图。','requestBody':paths['/v1/skins/generate']['post']['requestBody'],'responses':{'202':{'description':'已创建','content':{'application/json':{'schema':obj({'id':job_id,'state':string(enum=['running']),'expires_at':string(format='date-time')},['id','state','expires_at'])}}},'400':{'description':'描述无效'},'401':{'description':'需要认证'},'503':{'description':'未配置、容量已满或正在关闭（skin_jobs_busy），或任务存储暂不可用（job_unavailable）；均带 Retry-After'}}}}
paths['/v1/skins/jobs/{job}']={'parameters':[{'name':'job','in':'path','required':True,'schema':job_id}],
'get':{'summary':'查询插画任务及领取结果','tags':['皮肤'],'description':'running 时每 5 秒查询一次；succeeded 返回有界 PNG/JPEG 的 base64 数据；failed 不返回上游细节。执行任务的副本关闭或失联时任务以 failed 结束。不同主体、已删除、过期，或未启用数据库时服务重启后的任务均返回 404。','responses':{'200':{'description':'任务状态','content':{'application/json':{'schema':job_status}}},'401':{'description':'需要认证'},'404':{'description':'任务不存在或已失效'},'503':{'description':'任务存储暂不可用'}}},
'delete':{'summary':'取消或释放插画任务','tags':['皮肤'],'responses':{'204':{'description':'已释放；运行中的上游请求随即取消，任务在其他副本执行时最迟约 5 秒'},'401':{'description':'需要认证'},'404':{'description':'任务不存在或不属于当前主体'},'503':{'description':'任务存储暂不可用'}}}}

# 我的设备、无编码常用语同步与 App 内反馈（安卓重做批次一）。
def user_error_responses(codes):
    return {code:{'description':reason,'content':{'application/json':{'schema':{'$ref':'#/components/schemas/Error'}}}} for code,reason in codes}
session_item=obj({'id':string(pattern='^[0-9a-f]{64}$'),'platform':string(description='android、ios、macos、windows、linux、harmony，官网等浏览器登录为 web；无法识别时为空字符串。'),'name':string(description='设备名：客户端 User-Agent 里的设备型号，浏览器会话为「浏览器 · 系统」；无法识别时为空字符串。'),'app_version':string(description='客户端版本，只有 msime-<平台>/<版本> 格式的 User-Agent 才有，否则为空字符串。'),'created_at':string(format='date-time'),'last_active':string(format='date-time',description='最近一次刷新令牌的时间，由访问令牌到期时间推算。'),'current':{'type':'boolean','description':'是否为发出本请求的会话。'}},['id','platform','name','app_version','created_at','last_active','current'])
paths['/v1/users/me/sessions']={'get':{'summary':'列出我的登录设备','tags':['用户体系'],'security':[{'userSession':[]}],'description':'返回本人未撤销、未过期的会话，按最近活跃倒序最多 50 条。设备信息只来自登录请求的 User-Agent（登录时记录，刷新不更新），推荐格式 msime-<平台>/<版本> (<设备型号>; <系统>; edition=<版本 ID>)；旧客户端的 MSIME/Android 只能识别平台。匿名账号是另一个用户，不出现在真实账号的列表里。','responses':{'200':{'description':'成功','content':{'application/json':{'schema':obj({'sessions':{'type':'array','maxItems':50,'items':session_item}},['sessions'])}}},**user_error_responses([('401','需要有效用户会话'),('403','账号已封禁'),('429','请求过多'),('503','用户体系不可用')])}}}
paths['/v1/users/me/sessions/{id}']={'delete':{'summary':'移除一台登录设备','tags':['用户体系'],'security':[{'userSession':[]}],'parameters':[{'name':'id','in':'path','required':True,'schema':string(pattern='^[0-9a-f]{64}$')}],'description':'撤销本人的一个会话，该设备的访问令牌和刷新令牌立即失效；撤销当前会话等于退出登录。不要求最近登录。别人的、已撤销或已过期的会话都返回 404 session_not_found，不区分是否存在。','responses':{'204':{'description':'已撤销'},**user_error_responses([('401','需要有效用户会话'),('403','账号已封禁'),('404','会话不存在或不属于本人'),('429','请求过多'),('503','用户体系不可用')])}}}
phrase_item=obj({'id':string(minLength=1,maxLength=64,pattern='^[A-Za-z0-9_-]{1,64}$',description='客户端生成的 ID（推荐 UUID），列表内唯一。'),'text':string(minLength=1,maxLength=2000,description='1–2000 个 UTF-16 单元，可以换行，不含 NUL。'),'group':string(maxLength=32,description='分组名，最多 32 个 UTF-16 单元，不含控制字符；空字符串表示未分组。'),'position':{'type':'integer','format':'int64','minimum':0,'maximum':1000000,'description':'客户端的排序键。'}},['id','text','group','position'],True)
phrases_document=obj({'revision':{'type':'integer','format':'int64','minimum':0},'phrases':{'type':'array','maxItems':500,'items':phrase_item}},['revision','phrases'],True)
phrase_errors=user_error_responses([('400','列表无效（invalid_phrases）或 JSON 无效'),('401','需要有效用户会话'),('403','账号已封禁'),('409','revision_conflict：版本已变化，先重新读取'),('413','请求体超过 256 KiB'),('415','需要 application/json'),('429','请求过多'),('503','用户数据服务不可用')])
paths['/v1/users/me/phrases']={
 'get':{'summary':'读取无编码常用语','tags':['用户同步数据'],'security':[{'userSession':[]}],'description':'新用户返回 revision=0 和空列表。常用语是用户自己写的文本，只在用户打开同步后由客户端上传；注销账号会级联删除。','responses':{'200':{'description':'成功','content':{'application/json':{'schema':phrases_document}}},**phrase_errors}},
 'put':{'summary':'替换无编码常用语','tags':['用户同步数据'],'security':[{'userSession':[]}],'description':'整份替换，与偏好同一套 CAS：revision 必须等于当前版本，否则返回 409 revision_conflict；成功后版本加一。最多 500 条，请求体最多 256 KiB，未知字段返回 400。','requestBody':{'required':True,'content':{'application/json':{'schema':phrases_document}}},'responses':{'200':{'description':'成功，返回新文档','content':{'application/json':{'schema':phrases_document}}},**phrase_errors}}}
feedback_payload=obj({'type':string(enum=['bug','suggestion','dictionary']),'text':string(minLength=1,maxLength=500,description='去掉首尾空白后 1–500 个字符（UTF-16 单元），可以换行。'),'platform':string(enum=['android','ios','macos','windows','linux','harmony']),'app_version':string(minLength=1,maxLength=64),'edition':string(maxLength=32,pattern='^[a-z0-9._-]{0,32}$',description='产品版本 ID，例如 full、wubi；可为空字符串。'),'diagnostics':obj({k:string(maxLength=256) for k in ['device','os','app_version','edition','scheme','keyboard_layout','skin','ime_enabled','ime_default']},strict=True)},['type','text','platform','app_version'],True)
feedback_payload['properties']['diagnostics']['description']='可选，只在用户打开「附带诊断信息」时发送。只允许列出的键，值是不超过 256 字节的单行字符串，不能包含输入内容。'
paths['/v1/feedback']={'post':{'summary':'提交 App 内反馈','tags':['用户体系'],'security':[{'userSession':[]}],'description':'需要用户会话，匿名账号也可以提交。multipart/form-data：payload 为 JSON（最多 16 KiB，未知字段返回 400），screenshots 为 0–3 张 PNG/JPEG，每张最多 1 MiB、单边最多 8192、总像素最多 8 MP，整个请求最多约 3.1 MiB。服务端重新编码截图，去掉 EXIF 等元数据，截图存在数据库里，不进公开存储。限流：每个用户每小时 5 次、每天 20 次，每个地址每天 50 次（PostgreSQL auth_rates，所有副本共享）。反馈只给管理后台看，保留 180 天，注销账号时一并删除。正文、诊断和截图不写日志。','requestBody':{'required':True,'content':{'multipart/form-data':{'schema':obj({'payload':feedback_payload,'screenshots':{'type':'array','maxItems':3,'items':string(format='binary')}},['payload']),'encoding':{'payload':{'contentType':'application/json'},'screenshots':{'contentType':'image/png, image/jpeg'}}}}},'responses':{'201':{'description':'已收到','content':{'application/json':{'schema':obj({'id':string(format='uuid'),'status':string(enum=['received'])},['id','status'])}}},**user_error_responses([('400','payload 或截图无效：invalid_json、invalid_feedback、invalid_feedback_type、invalid_feedback_text、invalid_platform、invalid_app_version、invalid_edition、invalid_diagnostics、too_many_screenshots、invalid_screenshot'),('401','需要有效用户会话'),('403','账号已封禁'),('413','截图超过 1 MiB（screenshot_too_large）或请求过大（feedback_too_large）'),('415','需要 multipart/form-data'),('429','提交过于频繁'),('503','用户体系不可用或服务繁忙')])}}}

output=root/'internal/server/swagger/openapi.json'
data=json.dumps(result,ensure_ascii=False,indent=2)+'\n'
if '--check' in sys.argv:
    if not output.exists() or output.read_text()!=data:sys.exit('OpenAPI 未同步：请运行 python3 scripts/generate_openapi.py')
else:output.write_text(data)
print('OpenAPI 与契约一致' if '--check' in sys.argv else '已生成 OpenAPI')
