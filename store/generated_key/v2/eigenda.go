package eigenda

import (
	"context"
	"fmt"
	"time"

	"github.com/Layr-Labs/eigenda-proxy/common"
	"github.com/Layr-Labs/eigenda/api/clients/v2/coretypes"
	"github.com/Layr-Labs/eigensdk-go/logging"
	"github.com/avast/retry-go/v4"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/Layr-Labs/eigenda/api/clients/v2"
	"github.com/Layr-Labs/eigenda/api/clients/v2/verification"
	"github.com/ethereum/go-ethereum/rlp"
)

type Config struct {
	// cert verifier address used for verifying DA certificates
	// TODO: Support dynamic client injection https://github.com/Layr-Labs/eigenda-proxy/issues/307
	CertVerifierAddress string

	// maximum allowed blob size for dispersal. this is irrespective of the disperser's limits and could result in
	// failed payload submissions if set incorrectly
	MaxBlobSizeBytes uint64

	// number of times to retry an eigenda blob dispersal before yielding an error to the handler
	PutRetries uint
}

// Store does storage interactions and verifications for blobs with the EigenDA V2 protocol.
type Store struct {
	cfg Config
	log logging.Logger

	disperser *clients.PayloadDisperser
	retriever clients.PayloadRetriever
	verifier  verification.ICertVerifier
}

var _ common.GeneratedKeyStore = (*Store)(nil)

func NewStore(
	log logging.Logger,
	cfg Config,
	disperser *clients.PayloadDisperser,
	retriever clients.PayloadRetriever,
	verifier verification.ICertVerifier,
) (*Store, error) {
	return &Store{
		log:       log,
		cfg:       cfg,
		disperser: disperser,
		retriever: retriever,
		verifier:  verifier,
	}, nil
}

// Get fetches a blob from DA using certificate fields and verifies blob
// against commitment to ensure data is valid and non-tampered.
func (e Store) Get(ctx context.Context, key []byte) ([]byte, error) {
	var cert verification.EigenDACert
	err := rlp.DecodeBytes(key, &cert)
	if err != nil {
		return nil, fmt.Errorf("RLP decoding EigenDA v2 cert: %w", err)
	}

	payload, err := e.retriever.GetPayload(ctx, &cert)
	if err != nil {
		return nil, fmt.Errorf("getting payload: %w", err)
	}

	return payload.Serialize(), nil
}

// Put disperses a blob for some pre-image and returns the associated RLP encoded certificate commit.
// TODO: Client polling for different status codes, Mapping status codes to 503 failover
func (e Store) Put(ctx context.Context, value []byte) ([]byte, error) {
	e.log.Debug("Dispersing payload to EigenDA V2 network")

	// TODO: https://github.com/Layr-Labs/eigenda/issues/1271

	// We attempt to disperse the blob to EigenDA up to PutRetries times, unless we get a 400 error on any attempt.

	payload := coretypes.NewPayload(value)

	cert, err := retry.DoWithData(
		func() (*verification.EigenDACert, error) {
			return e.disperser.SendPayload(ctx, e.cfg.CertVerifierAddress, payload)
		},
		retry.RetryIf(
			func(err error) bool {
				grpcStatus, isGRPCError := status.FromError(err)
				if !isGRPCError {
					// api.ErrorFailover is returned, so we should retry
					return true
				}
				//nolint:exhaustive // we only care about a few grpc error codes
				switch grpcStatus.Code() {
				case codes.InvalidArgument:
					// we don't retry 400 errors because there is no point, we are passing invalid data
					return false
				case codes.ResourceExhausted:
					// we retry on 429s because *can* mean we are being rate limited
					// we sleep 1 second... very arbitrarily, because we don't have more info.
					// grpc error itself should return a backoff time,
					// see https://github.com/Layr-Labs/eigenda/issues/845 for more details
					time.Sleep(1 * time.Second)
					return true
				default:
					return true
				}
			}),
		// only return the last error. If it is an api.ErrorFailover, then the handler will convert
		// it to an http 503 to signify to the client (batcher) to failover to ethda b/c eigenda is temporarily down.
		retry.LastErrorOnly(true),
		retry.Attempts(e.cfg.PutRetries),
	)
	if err != nil {
		// TODO: we will want to filter for errors here and return a 503 when needed, i.e. when dispersal itself failed,
		//  or that we timed out waiting for batch to land onchain
		return nil, err
	}

	return rlp.EncodeToBytes(cert)
}

// BackendType returns the backend type for EigenDA Store
func (e Store) BackendType() common.BackendType {
	return common.EigenDAV2BackendType
}

// Verify verifies an EigenDACert by calling the verifyEigenDACertV2 view function
//
// Since v2 methods for fetching a payload are responsible for verifying the received bytes against the certificate,
// this Verify method only needs to check the cert on chain. That is why the third parameter is ignored.
func (e Store) Verify(ctx context.Context, certBytes []byte, _ []byte) error {
	var eigenDACert verification.EigenDACert
	err := rlp.DecodeBytes(certBytes, eigenDACert)
	if err != nil {
		return fmt.Errorf("RLP decoding EigenDA v2 cert: %w", err)
	}

	return e.verifier.VerifyCertV2(ctx, e.cfg.CertVerifierAddress, &eigenDACert)
}
