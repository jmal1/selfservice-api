package handlers

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/jmal1/selfservice-api/internal/audit"
	"github.com/jmal1/selfservice-api/internal/middleware"
	"github.com/jmal1/selfservice-api/internal/models"
)

var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
}

// VMConsoleWS upgrades to WebSocket and proxies traffic to the ESXi WebMKS endpoint.
// Authentication is via the session cookie (same as all other API endpoints).
func (h *Handler) VMConsoleWS(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := middleware.UserIDFromContext(ctx)
	role := middleware.RoleFromContext(ctx)

	// Configure origin check using allowed origins from config
	upgrader := wsUpgrader
	upgrader.CheckOrigin = func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true
		}
		for _, allowed := range h.allowedOrigins {
			if origin == allowed {
				return true
			}
		}
		return false
	}

	// Parse and validate IDs
	podID, err := uuid.Parse(chi.URLParam(r, "podID"))
	if err != nil {
		http.Error(w, "invalid pod id", http.StatusBadRequest)
		return
	}
	vmID, err := uuid.Parse(chi.URLParam(r, "vmID"))
	if err != nil {
		http.Error(w, "invalid vm id", http.StatusBadRequest)
		return
	}

	// Verify pod ownership
	pod, err := h.db.GetPodByID(ctx, podID)
	if err != nil {
		h.logger.Error("console: get pod failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if pod == nil {
		http.Error(w, "pod not found", http.StatusNotFound)
		return
	}
	if pod.OwnerID != userID && role != models.RoleAdmin {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	// Get VM and verify it belongs to this pod
	vm, err := h.db.GetPodVM(ctx, vmID)
	if err != nil {
		h.logger.Error("console: get vm failed", "error", err)
		http.Error(w, "vm not found", http.StatusNotFound)
		return
	}
	if vm.PodID != podID {
		http.Error(w, "vm does not belong to this pod", http.StatusForbidden)
		return
	}
	if vm.VCenterVMID == nil || *vm.VCenterVMID == "" {
		http.Error(w, "vm has no vCenter reference", http.StatusBadRequest)
		return
	}

	// Check vCenter client is available
	if h.vc == nil {
		http.Error(w, "console not available (vCenter not configured)", http.StatusServiceUnavailable)
		return
	}

	// Acquire WebMKS ticket from vCenter
	ticket, err := h.vc.AcquireWebMKSTicket(ctx, *vm.VCenterVMID)
	if err != nil {
		h.logger.Error("console: acquire ticket failed", "error", err, "moref", *vm.VCenterVMID)
		http.Error(w, "failed to acquire console ticket", http.StatusInternalServerError)
		return
	}

	// Audit: console opened
	audit.Log(ctx, h.db, "console.open",
		audit.Resource("vm", vmID),
		audit.IP(r.RemoteAddr),
		audit.Detail("vm_name", vm.DisplayName),
		audit.Detail("moref", *vm.VCenterVMID),
	)

	// Connect to ESXi WebMKS endpoint
	esxiURL := fmt.Sprintf("wss://%s:%d/ticket/%s", ticket.Host, ticket.Port, ticket.Ticket)
	h.logger.Info("console: connecting to ESXi", "url", esxiURL, "user", userID, "vm", vm.DisplayName)

	esxiDialer := websocket.Dialer{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		Subprotocols:    []string{"binary"},
	}

	// Pass through subprotocols from the client request
	clientProtocols := websocket.Subprotocols(r)
	if len(clientProtocols) > 0 {
		esxiDialer.Subprotocols = clientProtocols
	}

	esxiConn, _, err := esxiDialer.DialContext(ctx, esxiURL, nil)
	if err != nil {
		h.logger.Error("console: ESXi dial failed", "error", err, "url", esxiURL)
		http.Error(w, "failed to connect to VM console", http.StatusBadGateway)
		return
	}
	defer esxiConn.Close()

	// Upgrade client connection to WebSocket
	// Pass through the negotiated subprotocol from ESXi
	responseHeader := http.Header{}
	if sp := esxiConn.Subprotocol(); sp != "" {
		responseHeader.Set("Sec-WebSocket-Protocol", sp)
	}
	clientConn, err := upgrader.Upgrade(w, r, responseHeader)
	if err != nil {
		h.logger.Error("console: client upgrade failed", "error", err)
		return // Upgrade already sent error response
	}
	defer clientConn.Close()

	h.logger.Info("console: session started", "user", userID, "vm", vm.DisplayName, "pod", pod.Name)

	h.logger.Info("console: proxy starting",
		"user", userID,
		"vm", vm.DisplayName,
		"esxi_subprotocol", esxiConn.Subprotocol(),
		"client_subprotocol", clientConn.Subprotocol(),
	)

	// Bidirectional proxy
	var wg sync.WaitGroup
	wg.Add(2)

	// Client → ESXi
	go func() {
		defer wg.Done()
		err := proxyWS(clientConn, esxiConn)
		h.logger.Info("console: client→esxi closed", "error", err, "vm", vm.DisplayName)
		esxiConn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	}()

	// ESXi → Client
	go func() {
		defer wg.Done()
		err := proxyWS(esxiConn, clientConn)
		h.logger.Info("console: esxi→client closed", "error", err, "vm", vm.DisplayName)
		clientConn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	}()

	wg.Wait()

	// Audit: console closed
	audit.Log(context.Background(), h.db, "console.close",
		audit.Resource("vm", vmID),
		audit.IP(r.RemoteAddr),
		audit.Detail("vm_name", vm.DisplayName),
		audit.Detail("moref", *vm.VCenterVMID),
	)

	h.logger.Info("console: session ended", "user", userID, "vm", vm.DisplayName)
}

// proxyWS copies messages from src to dst until an error occurs.
func proxyWS(src, dst *websocket.Conn) error {
	for {
		msgType, reader, err := src.NextReader()
		if err != nil {
			return fmt.Errorf("NextReader: %w", err)
		}
		writer, err := dst.NextWriter(msgType)
		if err != nil {
			return fmt.Errorf("NextWriter: %w", err)
		}
		if _, err := io.Copy(writer, reader); err != nil {
			return fmt.Errorf("Copy: %w", err)
		}
		if err := writer.Close(); err != nil {
			return fmt.Errorf("WriterClose: %w", err)
		}
	}
}
