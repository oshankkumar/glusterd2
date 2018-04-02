package volumecommands

import (
	"net/http"

	"github.com/gluster/glusterd2/glusterd2/brick"
	"github.com/gluster/glusterd2/glusterd2/daemon"
	"github.com/gluster/glusterd2/glusterd2/events"
	"github.com/gluster/glusterd2/glusterd2/gdctx"
	restutils "github.com/gluster/glusterd2/glusterd2/servers/rest/utils"
	"github.com/gluster/glusterd2/glusterd2/transaction"
	"github.com/gluster/glusterd2/glusterd2/volume"
	"github.com/gluster/glusterd2/pkg/errors"

	"github.com/gorilla/mux"
	log "github.com/sirupsen/logrus"
)

func stopBricks(c transaction.TxnCtx) error {

	var volinfo volume.Volinfo
	if err := c.Get("volinfo", &volinfo); err != nil {
		return err
	}

	for _, b := range volinfo.GetLocalBricks() {
		brickDaemon, err := brick.NewGlusterfsd(b)
		if err != nil {
			return err
		}

		c.Logger().WithFields(log.Fields{
			"volume": volinfo.Name, "brick": b.String()}).Info("Stopping brick")

		client, err := daemon.GetRPCClient(brickDaemon)
		if err != nil {
			c.Logger().WithError(err).WithField(
				"brick", b.String()).Error("failed to connect to brick, sending SIGTERM")
			daemon.Stop(brickDaemon, false)
			continue
		}

		req := &brick.GfBrickOpReq{
			Name: b.Path,
			Op:   int(brick.OpBrickTerminate),
		}
		var rsp brick.GfBrickOpRsp
		err = client.Call("Brick.OpBrickTerminate", req, &rsp)
		if err != nil || rsp.OpRet != 0 {
			c.Logger().WithError(err).WithField(
				"brick", b.String()).Error("failed to send terminate RPC, sending SIGTERM")
			daemon.Stop(brickDaemon, false)
			continue
		}

		// On graceful shutdown of brick, daemon.Stop() isn't called.
		if err := daemon.DelDaemon(brickDaemon); err != nil {
			log.WithFields(log.Fields{
				"name": brickDaemon.Name(),
				"id":   brickDaemon.ID(),
			}).WithError(err).Warn("failed to delete brick entry from store, it may be restarted on GlusterD restart")
		}
	}

	return nil
}

func registerVolStopStepFuncs() {
	transaction.RegisterStepFunc(stopBricks, "vol-stop.StopBricks")
}

func volumeStopHandler(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()
	logger := gdctx.GetReqLogger(ctx)
	volname := mux.Vars(r)["volname"]

	lock, unlock := transaction.CreateLockFuncs(volname)
	// Taking a lock outside the txn as volinfo.Nodes() must also
	// be populated holding the lock. See issue #510
	if err := lock(ctx); err != nil {
		if err == transaction.ErrLockTimeout {
			restutils.SendHTTPError(ctx, w, http.StatusConflict, err)
		} else {
			restutils.SendHTTPError(ctx, w, http.StatusInternalServerError, err)
		}
		return
	}
	defer unlock(ctx)

	volinfo, err := volume.GetVolume(volname)
	if err != nil {
		// TODO: Distinguish between volume not present (404) and
		// store access failure (503)
		restutils.SendHTTPError(ctx, w, http.StatusNotFound, errors.ErrVolNotFound)
		return
	}

	if volinfo.State == volume.VolStopped {
		restutils.SendHTTPError(ctx, w, http.StatusBadRequest, errors.ErrVolAlreadyStopped)
		return
	}

	txn := transaction.NewTxn(ctx)
	defer txn.Cleanup()

	txn.Steps = []*transaction.Step{
		{
			DoFunc: "vol-stop.StopBricks",
			Nodes:  volinfo.Nodes(),
		},
	}

	if err := txn.Ctx.Set("volinfo", volinfo); err != nil {
		restutils.SendHTTPError(ctx, w, http.StatusInternalServerError, err)
		return
	}

	if err := txn.Do(); err != nil {
		logger.WithError(err).WithField(
			"volume", volname).Error("transaction to stop volume failed")
		restutils.SendHTTPError(ctx, w, http.StatusInternalServerError, err)
		return
	}

	volinfo.State = volume.VolStopped
	if err := volume.AddOrUpdateVolumeFunc(volinfo); err != nil {
		restutils.SendHTTPError(ctx, w, http.StatusInternalServerError, err)
		return
	}

	events.Broadcast(newVolumeEvent(eventVolumeStopped, volinfo))
	restutils.SendHTTPResponse(ctx, w, http.StatusOK, volinfo)
}
