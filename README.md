# k8s-informers-feeds

Monitor Kubernetes allocating IPs to Pods using Informers K8S client API
and redistribute as notifications to other services via MQTT messages.

Following instructions are assuming usage or provided Github Codespace setup on this repo.


### Setup demo cluster and some apps

```bash
# Create a new cluster
kind create cluster --name cluster1

# check what we have got
kubectl get nodes
kubectl get pods --all-namespaces

# Create a new namespace
kubectl create namespace app1
# Deploy test app
kubectl create deployment --image=nginx --replicas=2 nginx --namespace app1
# Check the pods
kubectl get pods --namespace app1
# Check the IPs
kubectl get pods --namespace app1 -o wide

# one more app
kubectl create namespace app2
kubectl create deployment web --image=nginx --replicas=3 --namespace app2
kubectl get pods --namespace app2 -o wide

# later we wil scale up/down
kubectl scale deployment nginx --replicas=3 --namespace app1
kubectl scale deployment web --replicas=2 --namespace app2
# Check the pods
kubectl get pods --namespace app1 -o wide
kubectl get pods --namespace app2 -o wide
```
Summary:
* got cluster
* deployed some apps
* know how to check Pod IPs
* know how to scale deployments up/down

### Create MQTT broker with mTLS

```bash
# install mosquitto MQTT broker and clients
sudo apt update; sudo apt-get -y install mosquitto mosquitto-clients

# create a new directory for the certificates
mkdir -p ~/mosquitto/certs
cd ~/mosquitto/certs
# create a new CA
openssl genrsa -out ca.key 2048
openssl req -x509 -new -nodes -key ca.key -sha256 -days 1024 -out ca.crt -subj "/C=US/ST=CA/L=San Francisco/O=My Company/CN=ca.example.com"
# create a new server key
openssl genrsa -out server.key 2048
openssl req -new -key server.key -out server.csr -subj "/C=US/ST=CA/L=San Francisco/O=My Company/CN=localhost" -addext "subjectAltName = DNS:localhost,DNS:example.com,IP:192.168.1.1,IP:127.0.0.1"
openssl x509 -req -in server.csr -CA ca.crt -CAkey ca.key -CAcreateserial -out server.crt -days 500 --extfile <(echo "subjectAltName = DNS:localhost,DNS:example.com,IP:192.168.1.1,IP:127.0.0.1" )
openssl x509 -in server.crt -text -noout | grep CN
openssl x509 -in server.crt -text -noout | grep IP
openssl x509 -in server.crt -text -noout | grep DNS
# create a new client1 key
openssl genrsa -out client1.key 2048
openssl req -new -key client1.key -out client1.csr -subj "/C=US/ST=CA/L=San Francisco/O=My Company/CN=client1.example.com"
openssl x509 -req -in client1.csr -CA ca.crt -CAkey ca.key -CAcreateserial -out client1.crt -days 500
# create a new client2 key
openssl genrsa -out client2.key 2048
openssl req -new -key client2.key -out client2.csr -subj "/C=US/ST=CA/L=San Francisco/O=My Company/CN=client2.example.com"
openssl x509 -req -in client2.csr -CA ca.crt -CAkey ca.key -CAcreateserial -out client2.crt -days 500
# check certs
openssl x509 -in ca.crt -text -noout | grep CN
openssl x509 -in server.crt -text -noout | grep CN
openssl x509 -in client1.crt -text -noout | grep CN
openssl x509 -in client2.crt -text -noout | grep CN
# deploy certs
cd ~/mosquitto/certs
sudo mkdir -p /etc/mosquitto/certs
sudo cp ca.crt /etc/mosquitto/certs/
sudo cp server.crt /etc/mosquitto/certs/
sudo cp server.key /etc/mosquitto/certs/
sudo cp client1.crt /etc/mosquitto/certs/
sudo cp client1.key /etc/mosquitto/certs/
sudo cp client2.crt /etc/mosquitto/certs/
sudo cp client2.key /etc/mosquitto/certs/

# set permissions
sudo chown -R mosquitto:mosquitto /etc/mosquitto/certs
sudo chmod -R 755 /etc/mosquitto/certs
ls -la /etc/mosquitto/certs

# configuration - review and copy
cd  /workspaces/k8s-informers-feeds
cat ./conf/mosquitto.conf
sudo cp ./conf/mosquitto.conf /etc/mosquitto/
cat ./conf/aclfile
sudo cp ./conf/aclfile /etc/mosquitto/
find /etc/mosquitto

# start the broker
cd  /workspaces/k8s-informers-feeds
sudo mkdir -p /run/mosquitto && sudo chown mosquitto:mosquitto /run/mosquitto 
sudo service mosquitto stop
sudo service mosquitto start
sudo service mosquitto status

# troubleshooting, hopefully not needed ;-)
sudo service mosquitto stop
sudo mosquitto -c /etc/mosquitto/mosquitto.conf -v
```

### Test the broker

```bash
# check the broker

# subscribe to a topic test/topic as client2 (read-only client)
mosquitto_sub -h localhost -p 8883 --cafile ~/mosquitto/certs/ca.crt --cert ~/mosquitto/certs/client2.crt --key ~/mosquitto/certs/client2.key -t test/topic

# other terminal - publish some message to the topic test/topic as RW client1
mosquitto_pub -h localhost -p 8883 --cafile ~/mosquitto/certs/ca.crt --cert ~/mosquitto/certs/client1.crt --key ~/mosquitto/certs/client1.key -t test/topic -m "Hello World at $(date)"

# publish by RO client2 should fail (DENIED PUBLIUSH in mosquitto -v log)
mosquitto_pub -h localhost -p 8883 --cafile ~/mosquitto/certs/ca.crt --cert ~/mosquitto/certs/client2.crt --key ~/mosquitto/certs/client2.key -t test/topic -m "Hello World at $(date)"


# other console should receive the message
```

Summary:
* installed mosquitto MQTT broker and clients
* created a new CA
* issued cetificates for the server and clients
* deployed certs to the broker
* started the broker
* tested the broker
* subscribed to a topic as client2 (read-only client)
* published some message to the topic as client1 (read-write client)
* received the message in the other console

### Kubernetes Informers based Monitor

```bash
cd /workspaces/k8s-informers-feeds
go mod init k8s-informers-feeds
go mod tidy

# build - takes time (get a cup of coffee)
go build -o monitor .
./monitor --help

# start monitor connected to mqtt broker
./monitor -clusterId clu1 -mqttBroker ssl://127.0.0.1:8883 -ca ~/mosquitto/certs/ca.crt -cert ~/mosquitto/certs/client1.crt -key ~/mosquitto/certs/client1.key -listenOn :7788
# other terminal - check the monitor events coming from the cluster on MQTT
mosquitto_sub -h localhost -p 8883 --cafile ~/mosquitto/certs/ca.crt --cert ~/mosquitto/certs/client2.crt --key ~/mosquitto/certs/client2.key -t '#' -v

# other terminal - check the feeds on HTTP requests
# check feeds on port 7788
# all pods
curl localhost:7788/pods | jq .
# namespace app1
curl localhost:7788/ns/app1 | jq .
# specific deployment
kubectl get deploy -A
curl localhost:7788/app/web | jq .
curl localhost:7788/app/nginx | jq .
# feed is made by clusterId_namespace_app
curl localhost:7788/feed/clu1_app1_nginx | jq .

# now we subscribe to the feed and will start scaling up and down
# subscribe to the feed
mosquitto_sub -h localhost -p 8883 --cafile ~/mosquitto/certs/ca.crt --cert ~/mosquitto/certs/client2.crt --key ~/mosquitto/certs/client2.key -t '#' -v

# other terminal - scale up
kubectl scale deployment nginx --replicas=5 --namespace app1
kubectl scale deployment nginx --replicas=2 --namespace app1

# notice messages prefix
# k8s-informers/refresh/feed/
# full MQTT message: k8s-informers/refresh/feed/clu1_app1_nginx {"add":"","delete":"10.244.0.5"}
```

Summary:
* built the monitor from source
* familiar with monitor's command line arguments
* connected the monitor to the MQTT broker using mTLS
* selected monitor HTTP port to read feeds
* checked state of Kubernetes IP lists of NS and Deployments
* reviewed feed update messages on MQTT


### Troubleshooting

```shell
### 
# hopefully not needed - TROUBLESHOOTING if mqtt broker refuses TLS connection - Test broker TLS on port 8883 with openssl s_client
openssl x509 -in ~/mosquitto/certs/server.crt -text -noout
openssl x509 -in ~/mosquitto/certs/server.crt -text -noout | grep CN
openssl x509 -in ~/mosquitto/certs/server.crt -text -noout | grep IP
openssl x509 -in ~/mosquitto/certs/server.crt -text -noout | grep DNS
openssl s_client -connect 127.0.0.1:8883 -servername localhost -CAfile ~/mosquitto/certs/ca.crt -cert ~/mosquitto/certs/client1.crt -key ~/mosquitto/certs/client1.key
```