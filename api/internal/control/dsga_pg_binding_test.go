package control

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	dsgapi "github.com/neondatabase/autoscaling/pkg/api/dsga"
)

// 真实专用TLS listener、签发器、typed Store与条件写；Kube/TokenReview为
// 协议夹具。新读者应保留无HWM时的首次PG身份，不重置或改写凭据。
// 沿用控制面已有标准testing，不为此引入另一套测试依赖。
func TestDSGABrokerReadsDurableFirstPGModelV2WithoutResetOrKeyWrite(t *testing.T) {
	f, store, claim, key, client, address := newBrokerTest(t)
	ctx := context.Background()
	before, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	id := dsgapi.Instance{VMUID: string(claim.VMUID), ComputeInstanceID: "compute-1", PGBootID: 11}
	bound, err := store.BindPG(ctx, before, before.State().Owner, id)
	if err != nil {
		t.Fatal(err)
	}
	if bound.State().ProtocolVersion != 2 || bound.State().PGInstance == nil || *bound.State().PGInstance != id || bound.State().HighWater != nil {
		t.Fatal("首次绑定没有保存完整v2身份或伪造HWM")
	}
	body, err := json.Marshal(claim)
	if err != nil {
		t.Fatal(err)
	}
	code, data := brokerRequest(t, client, address, body)
	if code != 200 {
		t.Fatalf("v2读者签发状态=%d", code)
	}
	var reply struct {
		AccessToken string `json:"access_token"`
		Epoch       string `json:"issuer_epoch"`
		Compute     string `json:"compute_instance_id"`
	}
	if err = json.Unmarshal(data, &reply); err != nil || reply.Epoch != "1" || reply.Compute != id.ComputeInstanceID {
		t.Fatal("签发身份改变")
	}
	parts := strings.Split(reply.AccessToken, ".")
	if len(parts) != 3 {
		t.Fatal("无有效JWT")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !ed25519.Verify(key.Public().(ed25519.PublicKey), []byte(parts[0]+"."+parts[1]), sig) {
		t.Fatal("签名无效")
	}
	id.PGBootID++
	if _, err = store.BindPG(ctx, bound, bound.State().Owner, id); err == nil {
		t.Fatal("凭据签发成功不能许可更换boot")
	}
	after, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(bound.State(), after.State()) {
		t.Fatal("读取/签发改写了原账本")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.validAPIAuth || f.secretWrites != 0 {
		t.Fatal("读取新账本改变了Kube身份或写入凭据")
	}
}
