package output

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/mic"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Status struct{ Detail string }

var listDevices = devices
var ClientName = mic.ClientName

// Run reconnects until cancelled. Discovery also retries so plugging in an
// output or installing a playback tool does not require restarting the TUI.
// report must not block (the TUI uses its conflating status forwarder).
func Run(ctx context.Context, svc fleetgrpc.FleetServiceClient, report func(Status)) {
	delay := time.Second
	for ctx.Err() == nil {
		tool, list, err := listDevices()
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			err = runStream(ctx, svc, tool, list, report)
		}
		if ctx.Err() != nil {
			return
		}
		if status.Code(err) == codes.Unimplemented {
			report(Status{"update the daemon to enable audio output"})
			return
		}
		if err != nil {
			report(Status{mic.CleanText(err.Error(), 200)})
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, 10*time.Second)
	}
}

func wireDevices(list []Device) *fleetgrpc.OutputDeviceList {
	out := &fleetgrpc.OutputDeviceList{}
	for _, d := range mic.CleanDevices(list) {
		out.Devices = append(out.Devices, &fleetgrpc.OutputDevice{Id: d.ID, Label: d.Label})
	}
	return out
}

func runStream(parent context.Context, svc fleetgrpc.FleetServiceClient, tool string, list []Device, report func(Status)) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	stream, err := svc.Output(ctx, grpc.WaitForReady(true))
	if err != nil {
		return err
	}
	if err := stream.Send(&fleetgrpc.OutputUp{Msg: &fleetgrpc.OutputUp_Open{Open: &fleetgrpc.OutputOpen{Client: ClientName()}}}); err != nil {
		return err
	}
	if err := stream.Send(&fleetgrpc.OutputUp{Msg: &fleetgrpc.OutputUp_Devices{Devices: wireDevices(list)}}); err != nil {
		return err
	}

	updates := make(chan *fleetgrpc.OutputDown, 8)
	errors := make(chan error, 1)
	relist := make(chan struct{}, 1)
	type listing struct {
		tool string
		list []Device
	}
	lists := make(chan listing, 1)
	enumerate := listDevices
	fail := func(err error) {
		select {
		case errors <- err:
		default:
		}
	}
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				select {
				case errors <- err:
				default:
				}
				return
			}
			if msg.GetListDevices() != nil {
				select {
				case relist <- struct{}{}:
				default:
				}
				continue
			}
			select {
			case updates <- msg:
			case <-ctx.Done():
				return
			}
		}
	}()
	// The only sender after the initial announcement. Enumeration never holds
	// up playback, selection changes or cancellation.
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			case <-relist:
			}
			t, ds, err := enumerate()
			if err != nil {
				// Release Auto to another client while local playback is unavailable.
				fail(err)
				return
			}
			if err := stream.Send(&fleetgrpc.OutputUp{Msg: &fleetgrpc.OutputUp_Devices{Devices: wireDevices(ds)}}); err != nil {
				fail(err)
				return
			}
			select {
			case <-lists:
			default:
			}
			select {
			case lists <- listing{t, ds}:
			case <-ctx.Done():
				return
			}
		}
	}()

	var p *player
	var playerDone <-chan struct{}
	var active bool
	var device string
	var useDefault bool
	args := func() ([]string, bool) {
		if useDefault {
			argv, _ := playbackArgs(tool, "", list)
			return argv, true
		}
		return playbackArgs(tool, device, list)
	}
	var retry, lastAudio time.Time
	mix := mixer{}
	stop := func() {
		if p != nil {
			p.stop()
			p = nil
			playerDone = nil
		}
	}
	defer stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	report(Status{"connected — waiting for output selection"})
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errors:
			return err
		case ds := <-lists:
			before, _ := args()
			tool, list = ds.tool, ds.list
			// Keep the requested device across fallback. A fresh listing retries
			// it and also moves an active default player back to a returned device.
			useDefault = false
			after, _ := args()
			if !slices.Equal(before, after) {
				stop()
				retry = time.Time{}
			}
		case msg := <-updates:
			if s := msg.GetSelection(); s != nil {
				stop()
				clear(mix)
				active, device = s.GetActive(), s.GetDevice()
				useDefault = false
				retry = time.Time{}
				if active {
					report(Status{"ready — waiting for instance audio"})
				} else {
					report(Status{"standby — another client is selected"})
				}
			}
			if a := msg.GetAudio(); a != nil && active {
				if len(a.GetInstance()) > 512 || len(a.GetPcm()) > 16*ChunkBytes {
					continue
				}
				// Drain already received samples on end; mixer.next removes the
				// instance once its bounded queue is empty.
				if !a.GetEnd() {
					mix.push(a.GetInstance(), a.GetPcm())
				}
			}
		case <-playerDone:
			stop()
			clear(mix)
			retry = time.Now().Add(5 * time.Second)
			// A failed named device uses the default until the next listing.
			// Do not lose the user's requested device or retry a failed default
			// player in a tight loop.
			_, alreadyFallback := args()
			if device != "" && !alreadyFallback {
				useDefault = true
				retry = time.Now()
				report(Status{"device unavailable — using system default"})
			} else {
				report(Status{"playback exited — retrying"})
			}
		case now := <-ticker.C:
			pcm := mix.next()
			if pcm == nil {
				if p != nil && now.Sub(lastAudio) > time.Second {
					stop()
					report(Status{"ready — waiting for instance audio"})
				}
				continue
			}
			lastAudio = now
			if p == nil && !now.Before(retry) {
				argv, fallback := args()
				if len(argv) == 0 {
					return fmt.Errorf("no playback command for %s", tool)
				}
				p, err = startPlayer(ctx, argv)
				if err != nil {
					report(Status{err.Error()})
					retry = now.Add(5 * time.Second)
					continue
				}
				playerDone = p.done
				if fallback {
					report(Status{"playing — selected device unavailable, using system default"})
				} else {
					report(Status{"playing instance audio"})
				}
			}
			if p != nil {
				p.write(pcm)
			}
		}
	}
}
