"""Generate versioned structural CRDs from this intentionally small API."""
import json
from pathlib import Path
root=Path(__file__).resolve().parents[1]
out=root/'deploy'
out.mkdir(exist_ok=True)
def string():return {'type':'string','minLength':1}
def integer(lo,hi,default=None):
    d={'type':'integer','minimum':lo,'maximum':hi}
    if default is not None:d['default']=default
    return d
pool={'compartmentId':string(),'subnetId':string(),'occupancy':integer(1,50,45),'maxShards':integer(1,100,2),'portStart':integer(1,65535,20000),'preserveSource':{'type':'boolean','default':False},'allocationMode':{'type':'string','enum':['Dynamic','Explicit']},'provisionEmpty':{'type':'boolean'},'maxWorkers':integer(1,512)}
binding={'pool':string(),'service':string(),'portName':string(),'mode':{'type':'string','enum':['NodePort','PodIP','NodePortCluster'],'default':'NodePort'},'healthPort':integer(1,65535),'requestedPort':integer(1,65535),'suspended':{'type':'boolean'}}
gatewaypool={'gatewayClassName':string(),'occupancy':integer(1,50,45),'maxGateways':integer(1,100,27),'portStart':integer(1024,65535,20000),'maxWorkers':integer(1,512)}
for kind,plural,props,required in [('NLBPool','nlbpools',pool,['compartmentId','subnetId','occupancy','maxShards','portStart','preserveSource']),('TunnelBinding','tunnelbindings',binding,['pool','service','portName','mode','healthPort']),('GatewayPool','gatewaypools',gatewaypool,['gatewayClassName','occupancy','maxGateways','portStart'])]:
    spec={'type':'object','properties':props,'required':required}
    # Freeze only ownership-routing fields; health port can be corrected operationally.
    fields=list(props) if kind!='TunnelBinding' else ['pool','service','portName','mode','requestedPort']
    spec['x-kubernetes-validations']=[{'rule':f'self.{k} == oldSelf.{k}' if k in required else f'has(self.{k}) == has(oldSelf.{k}) && (!has(self.{k}) || self.{k} == oldSelf.{k})','message':f'{k} is immutable; create a new resource for migration'} for k in fields]
    if kind=='NLBPool':
        spec['x-kubernetes-validations'].append({'rule':"has(self.allocationMode) && self.allocationMode == 'Explicit' ? self.maxShards == 1 && self.portStart == 1 : self.portStart >= 1024 && self.portStart + self.occupancy - 1 <= 65535",'message':'Explicit pools require maxShards=1 and portStart=1; Dynamic port range must fit 1024..65535'})
    if kind=='GatewayPool':
        spec['x-kubernetes-validations'].append({'rule':'self.portStart + self.occupancy - 1 <= 65535','message':'Listener port range must fit within 65535'})
    if kind!='TunnelBinding':
        spec['x-kubernetes-validations'].append({'rule':'self.occupancy * (has(self.maxWorkers) ? self.maxWorkers : 20) <= 1024','message':'occupancy times maxWorkers (default 20, including surge) must fit 1024 backends'})
    else:
        spec['x-kubernetes-validations'].append({'rule':"self.mode != 'NodePortCluster' || self.healthPort == 10256",'message':'NodePortCluster uses fixed kube-proxy health port 10256'})
    schema={'type':'object','properties':{'apiVersion':{'type':'string'},'kind':{'type':'string'},'metadata':{'type':'object'},'spec':spec,'status':{'type':'object','x-kubernetes-preserve-unknown-fields':True}},'required':['spec']}
    crd={'apiVersion':'apiextensions.k8s.io/v1','kind':'CustomResourceDefinition','metadata':{'name':plural+'.nlb.independent.dev'},'spec':{'group':'nlb.independent.dev','scope':'Namespaced','names':{'kind':kind,'listKind':kind+'List','plural':plural,'singular':plural[:-1]},'versions':[{'name':'v1alpha1','served':True,'storage':True,'schema':{'openAPIV3Schema':schema},'subresources':{'status':{}}}]}}
    columns = [('Per-NLB','integer','.spec.occupancy'),('Max-NLBs','integer','.spec.maxShards' if kind=='NLBPool' else '.spec.maxGateways'),('Used-Slots','integer','.status.used'),('Free-Slots','integer','.status.available')] if kind!='TunnelBinding' else [('Pool','string','.spec.pool'),('Public-IP','string','.status.externalIP'),('UDP-Port','integer','.status.externalPort'),('Configured','string',".status.conditions[?(@.type=='Configured')].status")]
    crd['spec']['versions'][0]['additionalPrinterColumns']=[dict(name=n,type=t,jsonPath=p) for n,t,p in columns+[('Age','date','.metadata.creationTimestamp')]]
    (out/(plural+'.json')).write_text(json.dumps(crd,indent=2)+'\n')
