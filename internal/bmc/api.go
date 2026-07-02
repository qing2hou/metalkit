package bmc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// API surface (all behind Basic Auth):
//
//   GET    /api/v1/bmc                       list metadata (no passwords)
//   POST   /api/v1/bmc                       create-by-IP: server derives placeholder UUID
//   GET    /api/v1/bmc/{machine_uuid}        one (no password)
//   PUT    /api/v1/bmc/{machine_uuid}        upsert (password optional on update)
//   DELETE /api/v1/bmc/{machine_uuid}        remove
//   POST   /api/v1/bmc/{machine_uuid}/test   run a `chassis power status` probe
//   POST   /api/v1/bmc/{machine_uuid}/power/{action}
//                                            on / off / cycle / soft / reset
//   POST   /api/v1/bmc/{machine_uuid}/onboard
//                                            bootdev=pxe + power cycle —— 让目标机
//                                            进 live 系统上报库存（不装系统）
//
// POST has a single purpose: the UI can register a BMC before the host has
// PXE'd (so no SMBIOS UUID exists yet). The server derives a deterministic
// placeholder UUID from the IP. PUT remains the editing entry point and works
// with both placeholder and real UUIDs.

// Tester is implemented by the ipmitool wrapper. The handler is given the
// decrypted credential and is expected to return ("on"|"off"|"unknown", nil)
// on success or ("", err) when the BMC is unreachable. The power-action
// methods return nil on success or err on failure; their wire-level effect is
// fire-and-forget (the BMC ACKs the command, the host may take seconds to
// react).
//
// We don't pull `ipmi.Client` directly into this package to avoid an import
// cycle (ipmi already imports bmc for its credential type).
type Tester interface {
	PowerStatus(ctx context.Context, cred PasswordedCredential) (string, error)
	PowerOn(ctx context.Context, cred PasswordedCredential) error
	PowerOff(ctx context.Context, cred PasswordedCredential) error
	PowerCycle(ctx context.Context, cred PasswordedCredential) error
	PowerSoft(ctx context.Context, cred PasswordedCredential) error
	PowerReset(ctx context.Context, cred PasswordedCredential) error
	// BootForPXE 设 next-boot=pxe 然后 power cycle。一次性，目标机走完
	// live 后下次再重启就回到正常引导链（不会反复 PXE）。
	BootForPXE(ctx context.Context, cred PasswordedCredential) error
}

// API binds the store to HTTP handlers.
type API struct {
	store  *Store
	logger *slog.Logger
	tester Tester // optional; nil means /test returns 503
}

// NewAPI constructs an API.
func NewAPI(store *Store, logger *slog.Logger) *API {
	return &API{store: store, logger: logger}
}

// WithTester wires an ipmi tester for the POST /test endpoint. If unset, the
// endpoint returns 503 ("ipmi not available on this controller").
func (a *API) WithTester(t Tester) *API {
	a.tester = t
	return a
}

// RegisterRoutes mounts BMC endpoints on mux.
func (a *API) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/bmc", a.list)
	mux.HandleFunc("POST /api/v1/bmc", a.create)
	mux.HandleFunc("GET /api/v1/bmc/{uuid}", a.get)
	mux.HandleFunc("PUT /api/v1/bmc/{uuid}", a.upsert)
	mux.HandleFunc("DELETE /api/v1/bmc/{uuid}", a.delete)
	mux.HandleFunc("POST /api/v1/bmc/{uuid}/test", a.test)
	mux.HandleFunc("POST /api/v1/bmc/{uuid}/power/{action}", a.power)
	mux.HandleFunc("POST /api/v1/bmc/{uuid}/onboard", a.onboard)
}

func (a *API) list(w http.ResponseWriter, r *http.Request) {
	cs, err := a.store.List(r.Context())
	if err != nil {
		a.logger.Error("list bmc", "err", err)
		writeError(w, http.StatusInternalServerError, "list failed")
		return
	}
	writeJSON(w, http.StatusOK, cs)
}

func (a *API) get(w http.ResponseWriter, r *http.Request) {
	uuid := strings.ToLower(strings.TrimSpace(r.PathValue("uuid")))
	c, err := a.store.Get(r.Context(), uuid)
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "bmc credential not found")
		return
	}
	if err != nil {
		if strings.Contains(err.Error(), "machine_uuid") {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		a.logger.Error("get bmc", "err", err, "uuid", uuid)
		writeError(w, http.StatusInternalServerError, "fetch failed")
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (a *API) upsert(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8*1024)
	var in UpsertInput
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	in.MachineUUID = r.PathValue("uuid")
	in.UpdatedBy = basicAuthUser(r)

	c, err := a.store.Upsert(r.Context(), in)
	switch {
	case errors.Is(err, ErrMachineUnknown):
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	case err != nil:
		var conflict ErrIPConflict
		if errors.As(err, &conflict) {
			writeError(w, http.StatusConflict,
				fmt.Sprintf("bmc ip %q 已被机器 %s 注册，请确认是否同一台机器",
					conflict.IP, conflict.Existing))
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// create accepts {ip, port?, username, password, ipmi_interface?, name?} and
// derives a placeholder machine_uuid from the IP. Use this when the host has
// not yet PXE'd and no SMBIOS UUID is known. The placeholder is later
// reconciled by inventory.UpsertReport when the real machine reports in.
//
//   201  on create (returns the credential with the derived machine_uuid)
//   409  if another BMC is already registered at the same IP
//   400  on validation error
func (a *API) create(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8*1024)
	var in UpsertInput
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	in.UpdatedBy = basicAuthUser(r)

	// Validate the IP first so the derived uuid is well-formed; everything
	// else gets re-validated inside Upsert.
	ip, err := validateIP(in.IP)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	in.IP = ip

	// Refuse if an existing credential (real or placeholder) already owns this IP.
	if existing, err := a.store.FindByIP(r.Context(), ip); err == nil && existing != "" {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":               "BMC already registered at this IP",
			"existing_machine_uuid": existing,
		})
		return
	}

	in.MachineUUID = PlaceholderUUID(ip)
	c, err := a.store.Upsert(r.Context(), in)
	if err != nil {
		// ErrMachineUnknown can't happen here (Upsert auto-creates placeholder
		// machines), so any error is a validation/encryption issue → 400.
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func (a *API) delete(w http.ResponseWriter, r *http.Request) {
	uuid := r.PathValue("uuid")
	if err := a.store.Delete(r.Context(), uuid); err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(w, http.StatusNotFound, "bmc credential not found")
			return
		}
		if strings.Contains(err.Error(), "machine_uuid") {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		a.logger.Error("delete bmc", "err", err, "uuid", uuid)
		writeError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// test runs `ipmitool chassis power status` against the stored credential.
// Behaviour:
//   - 200 {"ok":true, "power":"on"|"off"|"unknown"}  on a successful probe
//   - 200 {"ok":false, "error":"<msg>"}              on ipmitool failure
//     (NOT 5xx — the UI wants to render the message inline, not have apiSend throw)
//   - 404                                            no BMC for this uuid
//   - 503                                            controller has no ipmi tester wired
func (a *API) test(w http.ResponseWriter, r *http.Request) {
	if a.tester == nil {
		writeError(w, http.StatusServiceUnavailable, "ipmi not available on this controller")
		return
	}
	uuid := strings.ToLower(strings.TrimSpace(r.PathValue("uuid")))
	cred, err := a.store.GetWithPassword(r.Context(), uuid)
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "bmc credential not found")
		return
	}
	if err != nil {
		if strings.Contains(err.Error(), "machine_uuid") {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		a.logger.Error("test bmc: load cred", "err", err, "uuid", uuid)
		writeError(w, http.StatusInternalServerError, "load failed")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	power, perr := a.tester.PowerStatus(ctx, *cred)
	if perr != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": perr.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "power": power})
}

// power runs `ipmitool chassis power <action>` against the stored credential.
// Actions: on / off / cycle / soft / reset. Same response shape as /test —
// success/failure both return 200 so the UI can render errors inline.
//   200 {"ok":true, "action":"<action>"}
//   200 {"ok":false, "error":"<msg>"}
//   400 unknown action
//   404 no BMC for this uuid
//   503 controller has no ipmi tester wired
func (a *API) power(w http.ResponseWriter, r *http.Request) {
	if a.tester == nil {
		writeError(w, http.StatusServiceUnavailable, "ipmi not available on this controller")
		return
	}
	action := strings.ToLower(strings.TrimSpace(r.PathValue("action")))
	switch action {
	case "on", "off", "cycle", "soft", "reset":
	default:
		writeError(w, http.StatusBadRequest, "unknown action: "+action)
		return
	}
	uuid := strings.ToLower(strings.TrimSpace(r.PathValue("uuid")))
	cred, err := a.store.GetWithPassword(r.Context(), uuid)
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "bmc credential not found")
		return
	}
	if err != nil {
		if strings.Contains(err.Error(), "machine_uuid") {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		a.logger.Error("power bmc: load cred", "err", err, "uuid", uuid)
		writeError(w, http.StatusInternalServerError, "load failed")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	var perr error
	switch action {
	case "on":
		perr = a.tester.PowerOn(ctx, *cred)
	case "off":
		perr = a.tester.PowerOff(ctx, *cred)
	case "cycle":
		perr = a.tester.PowerCycle(ctx, *cred)
	case "soft":
		perr = a.tester.PowerSoft(ctx, *cred)
	case "reset":
		perr = a.tester.PowerReset(ctx, *cred)
	}
	if perr != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": perr.Error()})
		return
	}
	a.logger.Info("bmc power action", "uuid", uuid, "action", action, "by", basicAuthUser(r))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "action": action})
}

// onboard sets next-boot=pxe and power-cycles the target so it boots into
// metalkit's live image and reports inventory. **不装系统** —— 没有 binding
// 的话，agent claim 不到任何 job，只是把硬件信息上报回来。
// 主要用途：占位 BMC 注册之后，把目标机拉进 live 让它"露脸"，自动迁移占位 UUID
// 到真实 SMBIOS UUID。也可用于真实 UUID 机器的"重新发现 / 诊断"。
//
// 响应同 /test 和 /power：ipmi 调用失败也返 200 + {ok:false,error:...}。
//
//   200 {"ok":true,  "action":"onboard"}     on a successful boot kick
//   200 {"ok":false, "error":"<msg>"}        on ipmitool failure
//   404                                      no BMC for this uuid
//   503                                      controller has no ipmi tester wired
func (a *API) onboard(w http.ResponseWriter, r *http.Request) {
	if a.tester == nil {
		writeError(w, http.StatusServiceUnavailable, "ipmi not available on this controller")
		return
	}
	uuid := strings.ToLower(strings.TrimSpace(r.PathValue("uuid")))
	cred, err := a.store.GetWithPassword(r.Context(), uuid)
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "bmc credential not found")
		return
	}
	if err != nil {
		if strings.Contains(err.Error(), "machine_uuid") {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		a.logger.Error("onboard bmc: load cred", "err", err, "uuid", uuid)
		writeError(w, http.StatusInternalServerError, "load failed")
		return
	}

	// 20s：BootForPXE 内部要跑两次 ipmitool 子命令（set bootdev + power cycle），
	// 加 BMC 自己的 ACK 延迟，给一点余量。
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if perr := a.tester.BootForPXE(ctx, *cred); perr != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": perr.Error()})
		return
	}
	a.logger.Info("bmc onboard", "uuid", uuid, "ip", cred.IP, "by", basicAuthUser(r))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "action": "onboard"})
}

func basicAuthUser(r *http.Request) string {
	if u, _, ok := r.BasicAuth(); ok {
		return u
	}
	return "anonymous"
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
