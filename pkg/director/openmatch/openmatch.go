package openmatch

import (
	"context"
	"fmt"
	"github.com/Octops/agones-discover-openmatch/internal/runtime"
	"github.com/Octops/agones-discover-openmatch/pkg/allocator"
	"github.com/Octops/agones-discover-openmatch/pkg/config"
	"github.com/Octops/agones-discover-openmatch/pkg/director"
	"github.com/Octops/agones-discover-openmatch/pkg/extensions"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"io"
	"math/rand"
	"open-match.dev/open-match/pkg/pb"
	"time"
)

type Assigner interface {
	AssignTickets(ctx context.Context, in *pb.AssignTicketsRequest, opts ...grpc.CallOption) (*pb.AssignTicketsResponse, error)
}

type MatchFunctionServer struct {
	HostName string
	Port     int32
}

type FetchResponse struct {
	Matches []*pb.Match
	Err     error
}

type ConnFunc func() (*grpc.ClientConn, error)

func RunDirector(ctx context.Context, logger *logrus.Entry, dial ConnFunc, interval string, allocatorService allocator.AllocatorService) error {
	conn, err := dial()
	if err != nil {
		return errors.Wrap(err, "failed to connect to Open Match Backend")
	}

	defer conn.Close()
	client := pb.NewBackendServiceClient(conn)

	fetch := FetchMatches(client, MatchFunctionServer{
		HostName: config.OpenMatch().MatchFunctionHost,
		Port:     config.OpenMatch().MatchFunctionPort,
	})

	assign := AssignTickets(client, allocatorService)
	profiles := GenerateProfiles()

	if err := director.Run(interval)(ctx, profiles, fetch, assign); err != nil {
		logger.Error(errors.Wrap(err, "error running director"))
		return err
	}

	return nil
}

func FetchMatches(client pb.BackendServiceClient, matchFunctionServer MatchFunctionServer) director.FetchMatchesFunc {
	return func(ctx context.Context, profile *pb.MatchProfile) ([]*pb.Match, error) {
		logger := runtime.Logger().WithFields(logrus.Fields{
			"component": "director",
			"command":   "fetch",
		})
		ctxFetch, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()

		fetchResponse := FetchResponse{}
		go func(p *pb.MatchProfile) {
			defer cancel()
			if fetchResponse.Matches, fetchResponse.Err = fetchMatches(ctxFetch, client, profile, matchFunctionServer); fetchResponse.Err != nil {
				logger.Error(errors.Wrap(fetchResponse.Err, "failed to fetch matches from Open Match Backend"))
			}
		}(profile)

		<-ctxFetch.Done()
		return fetchResponse.Matches, fetchResponse.Err
	}
}

func AssignTickets(client pb.BackendServiceClient, allocatorService allocator.AllocatorService) director.AssignFunc {
	return func(ctx context.Context, matches []*pb.Match) error {
		logger := runtime.Logger().WithFields(logrus.Fields{
			"component": "director",
			"command":   "assign",
		})

		for _, match := range matches {
			req := CreateAssignTicketRequestForMatch(match)

			err := allocatorService.Allocate(ctx, req)
			if err != nil {
				err := errors.Wrapf(err, "failed to allocate servers for match %v", match.GetMatchId())
				logger.Error(err)
				return err
			}

			if err = assignTickets(ctx, req, client); err != nil {
				logger.Error(errors.Wrapf(err, "failed assign ticket for matchId %s", match.MatchId))
			}
		}

		return nil
	}
}

func CreateAssignTicketRequestForMatch(match *pb.Match) *pb.AssignTicketsRequest {
	var ticketIDs []string

	for _, t := range match.GetTickets() {
		ticketIDs = append(ticketIDs, t.Id)
	}

	req := &pb.AssignTicketsRequest{
		Assignments: []*pb.AssignmentGroup{
			{
				TicketIds: ticketIDs,
				Assignment: &pb.Assignment{
					// Extensions field is used by the allocator to extract the filter
					Extensions: match.Extensions,
				},
			},
		},
	}
	return req
}

func CleanUpAssignmentsWithoutConnection(group []*pb.AssignmentGroup) []*pb.AssignmentGroup {
	var cleanedGroup []*pb.AssignmentGroup

	for i := 0; i < len(group); i++ {
		if len(group[i].Assignment.Connection) > 0 {
			cleanedGroup = append(cleanedGroup, group[i])
			copy(group[i:], group[i+1:])
			group[len(group)-1] = nil // or the zero value of T
			group = group[:len(group)-1]
		}
	}

	return cleanedGroup
}

// generateProfiles generates profiles for every world assigning region, latency and skill randomly
func GenerateProfiles() director.GenerateProfilesFunc {
	return func() ([]*pb.MatchProfile, error) {
		var profiles []*pb.MatchProfile

		worlds := []string{"Dune", "Nova", "Pandora", "Orion"}
		regions := []string{"us-east-1", "us-east-2", "us-west-1", "us-west-2"}

		skillLevels := []*pb.DoubleRangeFilter{
			{DoubleArg: "skill", Min: 0, Max: 10},
			{DoubleArg: "skill", Min: 10, Max: 100},
			{DoubleArg: "skill", Min: 100, Max: 1000},
		}

		latencies := []*pb.DoubleRangeFilter{
			{DoubleArg: "latency", Min: 0, Max: 25},
			{DoubleArg: "latency", Min: 25, Max: 50},
			{DoubleArg: "latency", Min: 50, Max: 75},
			{DoubleArg: "latency", Min: 75, Max: 100},
		}

		for _, world := range worlds {
			region := TagFromStringSlice(regions)

			profile := &pb.MatchProfile{
				Name: fmt.Sprintf("world_based_profile_%s_%s", world, region),
				Pools: []*pb.Pool{
					{
						Name: "pool_mode_" + world,
						TagPresentFilters: []*pb.TagPresentFilter{
							{Tag: "mode.session"},
						},
						StringEqualsFilters: []*pb.StringEqualsFilter{
							{StringArg: "world", Value: world},
							{StringArg: "region", Value: region},
						},
						DoubleRangeFilters: []*pb.DoubleRangeFilter{
							DoubleRangeFilterFromSlice(skillLevels),
							DoubleRangeFilterFromSlice(latencies),
						},
					},
				},
			}

			// build filter extensions
			filter := extensions.AllocatorFilterExtension{
				Labels: map[string]string{
					"region": region,
					"world":  world,
				},
				Fields: map[string]string{
					"status.state": "Ready",
				},
			}

			// Multiples Extensions: extensions.WithAny(filter.Any()).WithAny(foo.Any()).WithAny(bar.Any()).Extensions()
			profile.Extensions = extensions.WithAny(filter.Any()).Extensions()
			profiles = append(profiles, profile)
		}

		return profiles, nil
	}
}

func assignTickets(ctx context.Context, req *pb.AssignTicketsRequest, assigner Assigner) error {
	// TODO: Check for AssignTicketsResponse.Failures
	assignments := CleanUpAssignmentsWithoutConnection(req.Assignments)

	if len(assignments) == 0 {
		return fmt.Errorf("the AssignTicketsRequest does not have assignments with connections set")
	}

	req.Assignments = assignments
	if _, err := assigner.AssignTickets(ctx, req); err != nil {
		return errors.Wrapf(err, "failed to assign tickets with BackendServiceClient")
	}

	return nil
}

func fetchMatches(ctx context.Context, client pb.BackendServiceClient, profile *pb.MatchProfile, matchFunctionServer MatchFunctionServer) ([]*pb.Match, error) {
	req := &pb.FetchMatchesRequest{
		Config: &pb.FunctionConfig{
			Host: matchFunctionServer.HostName,
			Port: matchFunctionServer.Port,
			Type: pb.FunctionConfig_GRPC,
		},
		Profile: profile,
	}

	logger := runtime.Logger().WithFields(logrus.Fields{
		"component": "director",
		"command":   "fetch",
	})

	logger.Debugf("fetching matches for profile %s", profile.GetName())
	stream, err := client.FetchMatches(ctx, req)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to fetch matches")
	}

	var result []*pb.Match
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}

		if err != nil {
			return nil, errors.Wrapf(err, "failed to receive matches from stream")
		}

		result = append(result, resp.GetMatch())
	}

	return result, nil
}

func TagFromStringSlice(tags []string) string {
	rand.Seed(time.Now().UTC().UnixNano())
	randomIndex := rand.Intn(len(tags))

	return tags[randomIndex]
}

func DoubleRangeFilterFromSlice(tags []*pb.DoubleRangeFilter) *pb.DoubleRangeFilter {
	rand.Seed(time.Now().UTC().UnixNano())
	randomIndex := rand.Intn(len(tags))

	return tags[randomIndex]
}
