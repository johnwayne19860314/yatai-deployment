# https://stackoverflow.com/questions/52369247/namespace-stuck-as-terminating-how-i-removed-it
export NAMESPACE=ingress-nginx
kubectl proxy &
kubectl get namespace $NAMESPACE -o json |$JQ '.spec = {"finalizers":[]}' >temp.json

curl -k -H "Content-Type: application/json" -X PUT --data-binary @temp.json 127.0.0.1:8001/api/v1/namespaces/$NAMESPACE/finalize

# revert after removing all stuck-in-terminating namespace
# pkill -9 -f "kubectl proxy"
# verify
# ps -ef | grep "kubectl proxy"  [https://116.236.72.194:4430](https://116.236.72.194:4430/) 