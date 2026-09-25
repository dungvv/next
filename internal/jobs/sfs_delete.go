package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/macro-inc/macro/pkg/events"
)

// internalAuthHeader is the x-internal-auth-key header the static file
// service's /internal routes expect (static_file_service_client::
// INTERNAL_ACCESS_HEADER).
const internalAuthHeader = "x-internal-auth-key"

// handleSFSDelete ports the sfs_deleter pubsub worker in
// services/email_service: delete the file from the static file service,
// then drop the email_attachments_sfs row. A 404 from SFS counts as success
// — the file is already gone.
func (d *deps) handleSFSDelete(ctx context.Context, env events.Envelope) error {
	var m sfsDeleteMessage
	if err := json.Unmarshal(env.Data, &m); err != nil {
		return fmt.Errorf("decode sfs_delete job: %w", err)
	}
	if _, err := uuid.Parse(m.DBID); err != nil {
		return fmt.Errorf("sfs_delete job bad db_id %q: %w", m.DBID, err)
	}
	if _, err := uuid.Parse(m.SFSID); err != nil {
		return fmt.Errorf("sfs_delete job bad sfs_id %q: %w", m.SFSID, err)
	}
	if d.jcfg.StaticFileServiceURL == "" {
		return fmt.Errorf("sfs_delete: STATIC_FILE_SERVICE_URL not configured")
	}
	log := slog.With("db_id", m.DBID, "sfs_id", m.SFSID)

	url := fmt.Sprintf("%s/internal/file/%s",
		strings.TrimSuffix(d.jcfg.StaticFileServiceURL, "/"), m.SFSID)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return fmt.Errorf("build sfs delete request: %w", err)
	}
	if d.jcfg.InternalAPIKey != "" {
		req.Header.Set(internalAuthHeader, d.jcfg.InternalAPIKey)
	}
	resp, err := d.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("sfs delete request: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		// delete_file propagates 404 as an error; the worker treats it as
		// success — the file is already gone.
		log.Info("sfs_delete: file already deleted from SFS")
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
	default:
		return fmt.Errorf("sfs delete returned HTTP %d", resp.StatusCode)
	}

	// email_db_client::attachments::sfs::delete_attachment_sfs
	if _, err := d.pools.MacroDB.Exec(ctx, `
		DELETE FROM email_attachments_sfs WHERE id = $1
	`, m.DBID); err != nil {
		return fmt.Errorf("delete email_attachments_sfs row: %w", err)
	}

	log.Info("sfs_delete: attachment deleted from sfs")
	return nil
}
