package vcenter

// ovf.go - OVA import for the image-upload feature.
//
// Instructors can upload a .ova appliance to turn into a template. An .ova is
// a tar archive whose FIRST entry (per the OVF spec) is the .ovf descriptor,
// followed by the disk/media files it references. Because a tar stream is
// sequential and non-seekable, we import in a single forward pass:
//
//  1. Read the leading .ovf descriptor.
//  2. CreateImportSpec + ImportVApp to obtain the NFC lease and the set of
//     FileItems vCenter expects us to upload.
//  3. For each subsequent tar entry, match it to a FileItem by name and stream
//     it up the lease. Missing/extra entries are handled explicitly.
//  4. Complete the lease.
//
// On any failure after the VM shell exists we Abort the lease as a best-effort
// cleanup, because orphaned half-imported VMs are a recurring problem here.

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/vmware/govmomi/nfc"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/ovf"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
)

// ErrNotAnOVA is returned when the input is not a tar archive whose first
// entry is an .ovf descriptor (e.g. a bare .ovf descriptor was supplied).
var ErrNotAnOVA = errors.New("input is not an OVA: expected a tar (.ova) stream whose first entry is the .ovf descriptor")

// OVAImportParams holds the inputs for ImportOVA.
type OVAImportParams struct {
	Reader       io.Reader
	Size         int64
	VMName       string
	FolderPath   string // inventory folder for the imported VM
	Datastore    string
	ResourcePool string
	Network      string // network name to map OVF networks onto
}

// ImportOVA imports a .ova (tar) stream and returns the moref of the created VM.
func (c *Client) ImportOVA(ctx context.Context, p OVAImportParams) (string, error) {
	if p.Reader == nil {
		return "", fmt.Errorf("ImportOVA: reader is required")
	}
	if p.VMName == "" {
		return "", fmt.Errorf("ImportOVA: VM name is required")
	}
	if p.Datastore == "" {
		return "", fmt.Errorf("ImportOVA: datastore is required")
	}
	if err := c.ensureConnected(ctx); err != nil {
		return "", err
	}

	tr := tar.NewReader(p.Reader)

	// The OVF spec guarantees the descriptor is the first tar entry. Anything
	// that is not a readable tar whose first entry is an .ovf is not an OVA.
	hdr, err := tr.Next()
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrNotAnOVA, err)
	}
	if !strings.EqualFold(path.Ext(hdr.Name), ".ovf") {
		return "", fmt.Errorf("%w: first archive entry %q is not an .ovf descriptor", ErrNotAnOVA, hdr.Name)
	}

	descriptor, err := io.ReadAll(tr)
	if err != nil {
		return "", fmt.Errorf("read OVF descriptor: %w", err)
	}

	env, err := ovf.Unmarshal(bytes.NewReader(descriptor))
	if err != nil {
		return "", fmt.Errorf("parse OVF descriptor: %w", err)
	}

	placementRequest := PlacementRequest{
		ResourcePoolPath: p.ResourcePool,
		DatastoreName:    p.Datastore,
		NetworkName:      p.Network,
	}
	placement, err := c.ResolvePlacement(ctx, placementRequest)
	if err != nil {
		return "", fmt.Errorf("select OVA placement: %w", err)
	}
	ds := placement.Datastore
	pool := placement.Pool

	var folder *object.Folder
	if p.FolderPath != "" {
		folder, err = c.finder.Folder(ctx, p.FolderPath)
		if err != nil {
			return "", fmt.Errorf("find folder %q: %w", p.FolderPath, err)
		}
	} else {
		folders, ferr := c.datacenter.Folders(ctx)
		if ferr != nil {
			return "", fmt.Errorf("resolve datacenter VM folder: %w", ferr)
		}
		folder = folders.VmFolder
	}

	nmap, err := c.ovfNetworkMapping(ctx, env, p.Network)
	if err != nil {
		return "", err
	}

	cisp := types.OvfCreateImportSpecParams{
		EntityName:       p.VMName,
		DiskProvisioning: string(types.OvfCreateImportSpecParamsDiskProvisioningTypeThin),
		NetworkMapping:   nmap,
	}

	mgr := ovf.NewManager(c.client.Client)
	spec, err := mgr.CreateImportSpec(ctx, string(descriptor), pool, ds, &cisp)
	if err != nil {
		return "", fmt.Errorf("create OVF import spec: %w", err)
	}
	if spec == nil {
		return "", fmt.Errorf("create OVF import spec: nil result")
	}
	if len(spec.Error) > 0 {
		return "", fmt.Errorf("OVF import spec rejected: %s", spec.Error[0].LocalizedMessage)
	}

	placementRequest.PinnedHostMoRef = placement.Identity.MoRef
	placementRequest.PinnedPoolMoRef = placement.PoolMoRef
	if _, err := c.ResolvePlacement(ctx, placementRequest); err != nil {
		return "", fmt.Errorf("revalidate OVA placement immediately before import: %w", err)
	}
	lease, err := pool.ImportVApp(ctx, spec.ImportSpec, folder, placement.Host)
	if err != nil {
		return "", fmt.Errorf("import vApp: %w", err)
	}
	abortLease := func() {
		abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_ = lease.Abort(abortCtx, nil)
	}

	// From here on a VM shell exists; abort the lease on any error so we don't
	// leave an orphan behind.
	info, err := lease.Wait(ctx, spec.FileItem)
	if err != nil {
		abortLease()
		return "", fmt.Errorf("wait for NFC lease: %w", err)
	}

	moref := info.Entity.Value

	// Drain per-item progress channels and send keepalive progress to the
	// lease. lease.Upload uses each FileItem as its progress sink (an unbuffered
	// channel), so without a running updater the very first Upload blocks
	// forever trying to report progress.
	updater := lease.StartUpdater(ctx, info)
	defer updater.Done()

	abort := func(cause error) (string, error) {
		abortLease()
		return "", cause
	}

	// Index the expected uploads by base file name so we can match them to tar
	// entries as we stream forward through the archive.
	remaining := make(map[string]nfc.FileItem, len(info.Items))
	for _, item := range info.Items {
		remaining[path.Base(item.Path)] = item
	}

	for {
		entry, terr := tr.Next()
		if terr == io.EOF {
			break
		}
		if terr != nil {
			return abort(fmt.Errorf("read OVA archive entry: %w", terr))
		}

		base := path.Base(entry.Name)
		item, ok := remaining[base]
		if !ok {
			// Manifest (.mf), certificate (.cert) or any other extra file the
			// import doesn't need — skip it.
			continue
		}

		// lease.Upload sets Method/Type/Progress itself based on the item; we
		// only supply the content length so the request body is framed exactly.
		if uerr := lease.Upload(ctx, item, tr, soap.Upload{ContentLength: item.Size}); uerr != nil {
			return abort(fmt.Errorf("upload %q: %w", base, uerr))
		}
		delete(remaining, base)
	}

	if len(remaining) > 0 {
		missing := make([]string, 0, len(remaining))
		for name := range remaining {
			missing = append(missing, name)
		}
		sort.Strings(missing)
		return abort(fmt.Errorf("OVA is missing file(s) referenced by the descriptor: %s", strings.Join(missing, ", ")))
	}

	if err := lease.Complete(ctx); err != nil {
		return abort(fmt.Errorf("complete NFC lease: %w", err))
	}
	if err := c.ValidateVMPlacement(ctx, moref, placement.Identity.MoRef); err != nil {
		return "", fmt.Errorf("OVA imported on invalid host: %w", err)
	}

	return moref, nil
}

// ovfNetworkMapping maps every network declared in the OVF descriptor onto the
// single target network name. An appliance with no NIC needs no mapping;
// otherwise the target standard portgroup is mandatory and already validated
// by ResolvePlacement.
func (c *Client) ovfNetworkMapping(ctx context.Context, env *ovf.Envelope, network string) ([]types.OvfNetworkMapping, error) {
	if env == nil || env.Network == nil || len(env.Network.Networks) == 0 {
		return nil, nil
	}
	if network == "" {
		return nil, errors.New("OVA declares a network but no target standard portgroup was provided")
	}

	net, err := c.finder.Network(ctx, network)
	if err != nil {
		return nil, fmt.Errorf("find network %q: %w", network, err)
	}
	ref := net.Reference()

	nmap := make([]types.OvfNetworkMapping, 0, len(env.Network.Networks))
	for _, n := range env.Network.Networks {
		nmap = append(nmap, types.OvfNetworkMapping{
			Name:    n.Name,
			Network: ref,
		})
	}
	return nmap, nil
}
