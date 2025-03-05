package store

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"github.com/Layr-Labs/eigenda-proxy/common"
	"github.com/Layr-Labs/eigenda-proxy/metrics"
	"github.com/Layr-Labs/eigenda-proxy/store/generated_key/eigenda"
	"github.com/Layr-Labs/eigenda-proxy/store/generated_key/memstore"
	"github.com/Layr-Labs/eigenda-proxy/store/generated_key/memstore/memconfig"
	memstore_v2 "github.com/Layr-Labs/eigenda-proxy/store/generated_key/memstore/v2"
	eigenda_v2 "github.com/Layr-Labs/eigenda-proxy/store/generated_key/v2"
	"github.com/Layr-Labs/eigenda-proxy/store/precomputed_key/redis"
	"github.com/Layr-Labs/eigenda-proxy/store/precomputed_key/s3"
	"github.com/Layr-Labs/eigenda-proxy/verify/v1"
	"github.com/Layr-Labs/eigenda/api/clients"
	clients_v2 "github.com/Layr-Labs/eigenda/api/clients/v2"
	"github.com/Layr-Labs/eigenda/api/clients/v2/relay"
	"github.com/Layr-Labs/eigenda/api/clients/v2/verification"
	common_eigenda "github.com/Layr-Labs/eigenda/common"
	"github.com/Layr-Labs/eigenda/common/geth"
	auth "github.com/Layr-Labs/eigenda/core/auth/v2"
	eigenda_eth "github.com/Layr-Labs/eigenda/core/eth"
	core_v2 "github.com/Layr-Labs/eigenda/core/v2"
	"github.com/Layr-Labs/eigenda/encoding/kzg/prover"
	"github.com/Layr-Labs/eigensdk-go/logging"
	"github.com/consensys/gnark-crypto/ecc/bn254"
	geth_common "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// StorageManagerBuilder centralizes dependency initialization.
// It ensures proper typing and avoids redundant logic scattered across functions.
type StorageManagerBuilder struct {
	ctx     context.Context
	log     logging.Logger
	metrics metrics.Metricer

	managerCfg     Config
	memConfig      *memconfig.SafeConfig
	v1VerifierCfg  verify.Config
	v1EdaClientCfg clients.EigenDAClientConfig
	v2ClientCfg    common.ClientConfigV2
	v2SecretCfg    common.SecretConfigV2

	// TODO: these values ought to be moved into configs, rather than being included individually
	putRetries       uint
	maxBlobSizeBytes uint
}

// NewStorageManagerBuilder creates a builder which knows how to build an IManager
func NewStorageManagerBuilder(
	ctx context.Context,
	log logging.Logger,
	metrics metrics.Metricer,
	managerConfig Config,
	v1VerifierCfg verify.Config,
	v1EdaClientCfg clients.EigenDAClientConfig,
	v2ClientCfg common.ClientConfigV2,
	v2SecretCfg common.SecretConfigV2,
	memConfig *memconfig.SafeConfig,
	putRetries uint,
	maxBlobSize uint,
) *StorageManagerBuilder {
	return &StorageManagerBuilder{
		ctx,
		log,
		metrics,
		managerConfig,
		memConfig,
		v1VerifierCfg,
		v1EdaClientCfg,
		v2ClientCfg,
		v2SecretCfg,
		putRetries,
		maxBlobSize,
	}
}

// Build builds the storage manager object
func (smb *StorageManagerBuilder) Build(ctx context.Context) (*Manager, error) {
	var err error
	var s3Store *s3.Store
	var redisStore *redis.Store
	var eigenDAV1Store, eigenDAV2Store common.GeneratedKeyStore

	if smb.managerCfg.S3Config.Bucket != "" {
		smb.log.Debug("Using S3 storage backend")
		s3Store, err = s3.NewStore(smb.managerCfg.S3Config)
	}

	if err != nil {
		return nil, err
	}

	if smb.managerCfg.RedisConfig.Endpoint != "" {
		smb.log.Debug("Using Redis storage backend")
		redisStore, err = redis.NewStore(&smb.managerCfg.RedisConfig)
	}

	if err != nil {
		return nil, err
	}

	if smb.v2ClientCfg.Enabled {
		smb.log.Debug("Using EigenDA V2 storage backend")
		eigenDAV2Store, err = smb.buildEigenDAV2Backend(ctx)
		if err != nil {
			return nil, err
		}
	}

	eigenDAV1Store, err = smb.buildEigenDAV1Backend(ctx)
	if err != nil {
		return nil, err
	}

	fallbacks := smb.buildSecondaries(smb.managerCfg.FallbackTargets, s3Store, redisStore)
	caches := smb.buildSecondaries(smb.managerCfg.CacheTargets, s3Store, redisStore)
	secondary := NewSecondaryManager(smb.log, smb.metrics, caches, fallbacks)

	if secondary.Enabled() { // only spin-up go routines if secondary storage is enabled
		smb.log.Debug("Starting secondary write loop(s)", "count", smb.managerCfg.AsyncPutWorkers)

		for i := 0; i < smb.managerCfg.AsyncPutWorkers; i++ {
			go secondary.WriteSubscriptionLoop(ctx)
		}
	}

	smb.log.Info(
		"Created storage backends",
		"eigenda_v1", eigenDAV1Store != nil,
		"eigenda_v2", eigenDAV2Store != nil,
		"s3", s3Store != nil,
		"redis", redisStore != nil,
		"read_fallback", len(fallbacks) > 0,
		"caching", len(caches) > 0,
		"async_secondary_writes", (secondary.Enabled() && smb.managerCfg.AsyncPutWorkers > 0),
		"verify_v1_certs", smb.v1VerifierCfg.VerifyCerts,
	)
	return NewManager(eigenDAV1Store, eigenDAV2Store, s3Store, smb.log, secondary, smb.v2ClientCfg.Enabled)
}

// buildSecondaries ... Creates a slice of secondary targets used for either read
// failover or caching
func (smb *StorageManagerBuilder) buildSecondaries(
	targets []string,
	s3Store common.PrecomputedKeyStore,
	redisStore *redis.Store,
) []common.PrecomputedKeyStore {
	stores := make([]common.PrecomputedKeyStore, len(targets))

	for i, target := range targets {
		//nolint:exhaustive // TODO: implement additional secondaries
		switch common.StringToBackendType(target) {
		case common.RedisBackendType:
			if redisStore == nil {
				panic(fmt.Sprintf("Redis backend not configured: %s", target))
			}
			stores[i] = redisStore
		case common.S3BackendType:
			if s3Store == nil {
				panic(fmt.Sprintf("S3 backend not configured: %s", target))
			}
			stores[i] = s3Store

		default:
			panic(fmt.Sprintf("Invalid backend target: %s", target))
		}
	}
	return stores
}

// buildEigenDAV2Backend ... Builds EigenDA V2 storage backend
func (smb *StorageManagerBuilder) buildEigenDAV2Backend(ctx context.Context) (common.GeneratedKeyStore, error) {
	// TODO: Figure out how to better manage the v1 verifier
	//  may make sense to live in some global kzg config that's passed down across EigenDA versions
	kzgProver, err := prover.NewProver(smb.v1VerifierCfg.KzgConfig, nil)
	if err != nil {
		return nil, fmt.Errorf("new KZG prover: %w", err)
	}

	if smb.memConfig != nil {
		return memstore_v2.New(smb.ctx, smb.log, smb.memConfig, kzgProver.Srs.G1)
	}

	ethClient, err := smb.buildEthClient()
	if err != nil {
		return nil, fmt.Errorf("build eth client: %w", err)
	}

	relayPayloadRetriever, err := smb.buildRelayPayloadRetriever(ethClient, kzgProver.Srs.G1)
	if err != nil {
		return nil, fmt.Errorf("build relay payload retriever: %w", err)
	}

	certVerifier, err := verification.NewCertVerifier(
		smb.log, ethClient, smb.v2ClientCfg.BlockNumberPollIntervalDuration)
	if err != nil {
		return nil, fmt.Errorf("new cert verifier: %w", err)
	}

	payloadDisperser, err := smb.buildPayloadDisperser(ctx, ethClient, kzgProver, certVerifier)
	if err != nil {
		return nil, fmt.Errorf("build payload disperser: %w", err)
	}

	v2Config := eigenda_v2.Config{
		CertVerifierAddress: smb.v2ClientCfg.EigenDACertVerifierAddress,
		MaxBlobSizeBytes:    uint64(smb.maxBlobSizeBytes),
		PutRetries:          smb.v2ClientCfg.PutRetries,
	}

	return eigenda_v2.NewStore(smb.log, v2Config, payloadDisperser, relayPayloadRetriever, certVerifier)
}

// buildEigenDAV1Backend ... Builds EigenDA V1 storage backend
func (smb *StorageManagerBuilder) buildEigenDAV1Backend(ctx context.Context) (common.GeneratedKeyStore, error) {
	verifier, err := verify.NewVerifier(&smb.v1VerifierCfg, smb.log)
	if err != nil {
		return nil, fmt.Errorf("new verifier: %w", err)
	}

	if smb.v1VerifierCfg.VerifyCerts {
		smb.log.Info("Certificate verification with Ethereum enabled")
	} else {
		smb.log.Warn("Certificate verification disabled. This can result in invalid EigenDA certificates being accredited.")
	}

	if smb.memConfig != nil {
		smb.log.Info("Using memstore backend for EigenDA V1")
		return memstore.New(ctx, verifier, smb.log, smb.memConfig)
	}
	// EigenDAV1 backend dependency injection
	var client *clients.EigenDAClient
	smb.log.Warn("Using EigenDA backend.. This backend type will be deprecated soon. Please migrate to V2.")
	client, err = clients.NewEigenDAClient(smb.log, smb.v1EdaClientCfg)
	if err != nil {
		return nil, err
	}

	return eigenda.NewStore(
		client,
		verifier,
		smb.log,
		&eigenda.StoreConfig{
			MaxBlobSizeBytes:     uint64(smb.maxBlobSizeBytes),
			EthConfirmationDepth: smb.v1VerifierCfg.EthConfirmationDepth,
			StatusQueryTimeout:   smb.v1EdaClientCfg.StatusQueryTimeout,
			PutRetries:           smb.putRetries,
		},
	)
}

func (smb *StorageManagerBuilder) buildEthClient() (common_eigenda.EthClient, error) {
	gethCfg := geth.EthClientConfig{
		RPCURLs: []string{smb.v1EdaClientCfg.EthRpcUrl},
	}

	ethClient, err := geth.NewClient(gethCfg, geth_common.Address{}, 0, smb.log)
	if err != nil {
		return nil, fmt.Errorf("create geth client: %w", err)
	}

	return ethClient, nil
}

func (smb *StorageManagerBuilder) buildRelayPayloadRetriever(
	ethClient common_eigenda.EthClient,
	g1Srs []bn254.G1Affine,
) (*clients_v2.RelayPayloadRetriever, error) {
	relayClient, err := smb.buildRelayClient(ethClient)
	if err != nil {
		return nil, fmt.Errorf("build relay client: %w", err)
	}

	relayPayloadRetriever, err := clients_v2.NewRelayPayloadRetriever(
		smb.log,
		//nolint:gosec // disable G404: this doesn't need to be cryptographically secure
		rand.New(rand.NewSource(time.Now().UnixNano())),
		smb.v2ClientCfg.RelayPayloadRetrieverCfg,
		relayClient,
		g1Srs)
	if err != nil {
		return nil, fmt.Errorf("new relay payload retriever: %w", err)
	}

	return relayPayloadRetriever, nil
}

func (smb *StorageManagerBuilder) buildRelayClient(ethClient common_eigenda.EthClient) (clients_v2.RelayClient, error) {
	reader, err := eigenda_eth.NewReader(smb.log, ethClient, "0x0", smb.v2ClientCfg.ServiceManagerAddress)
	if err != nil {
		return nil, fmt.Errorf("new eth reader: %w", err)
	}

	relayURLProvider, err := relay.NewRelayUrlProvider(ethClient, reader.GetRelayRegistryAddress())
	if err != nil {
		return nil, fmt.Errorf("new relay url provider: %w", err)
	}

	relayCfg := &clients_v2.RelayClientConfig{
		UseSecureGrpcFlag: smb.v2ClientCfg.DisperserClientCfg.UseSecureGrpcFlag,
		// we should never expect a message greater than our allowed max blob size.
		// 10% of max blob size is added for additional safety
		MaxGRPCMessageSize: smb.maxBlobSizeBytes + (smb.maxBlobSizeBytes / 10),
	}

	relayClient, err := clients_v2.NewRelayClient(relayCfg, smb.log, relayURLProvider)
	if err != nil {
		return nil, fmt.Errorf("new relay client: %w", err)
	}

	return relayClient, nil
}

func (smb *StorageManagerBuilder) buildPayloadDisperser(
	ctx context.Context,
	ethClient common_eigenda.EthClient,
	kzgProver *prover.Prover,
	certVerifier *verification.CertVerifier,
) (*clients_v2.PayloadDisperser, error) {
	signer, err := smb.buildLocalSigner(ctx, ethClient)
	if err != nil {
		return nil, fmt.Errorf("build local signer: %w", err)
	}

	disperserClient, err := clients_v2.NewDisperserClient(&smb.v2ClientCfg.DisperserClientCfg, signer, kzgProver, nil)
	if err != nil {
		return nil, fmt.Errorf("new disperser client: %w", err)
	}

	payloadDisperser, err := clients_v2.NewPayloadDisperser(
		smb.log,
		smb.v2ClientCfg.PayloadDisperserCfg,
		disperserClient,
		certVerifier)
	if err != nil {
		return nil, fmt.Errorf("new payload disperser: %w", err)
	}

	return payloadDisperser, nil
}

// buildLocalSigner attempts to check the pending balance of the created signer account. If the check fails, or if the
// balance is determined to be 0, the user is warned with a log. This method doesn't return an error based on this check:
// it's possible that a user could want to set up a signer before it's actually ready to be used
//
// TODO: the checks performed in this method could be improved in the future, e.g. by checking payment vault state,
// or by accessing the disperser accountant
func (smb *StorageManagerBuilder) buildLocalSigner(
	ctx context.Context,
	ethClient common_eigenda.EthClient,
) (core_v2.BlobRequestSigner, error) {
	signer, err := auth.NewLocalBlobRequestSigner(smb.v2SecretCfg.SignerPaymentKey)
	if err != nil {
		return nil, fmt.Errorf("new local blob request signer: %w", err)
	}

	accountID := crypto.PubkeyToAddress(signer.PrivateKey.PublicKey)
	pendingBalance, err := ethClient.PendingBalanceAt(ctx, accountID)

	switch {
	case err != nil:
		smb.log.Errorf("get pending balance for accountID %v: %v", accountID, err)
	case pendingBalance == nil:
		smb.log.Errorf(
			"get pending balance for accountID %v didn't return an error, but pending balance is nil", accountID)
	case pendingBalance.Sign() <= 0:
		smb.log.Warnf("pending balance for accountID %v is zero", accountID)
	}

	return signer, nil
}
