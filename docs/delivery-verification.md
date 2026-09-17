# Delivery-contract verification notes

Native requirement ownership: [INT-DEL-001–005](../specs/delivery-contracts.md).

| Requirement | Implemented test evidence |
| --- | --- |
| INT-DEL-001 source/version | `environment.TestDeliveryAssetIsResolvedSealedAndNeverGuessed`, `TestDeliveryAssetStrictness`, `adaptive.TestDeliveryCapabilityVersionDigestBoundsAndIsolation`, `TestCapabilityBuilderRejectsUnresolvedAssets`. |
| INT-DEL-002 validation | `delivery.TestContractAndBodyValidation`, `TestClosedOfflineSchemaProfile`, `TestStrictJSONBoundaries`, `FuzzDecodeJSON`, `sandboxd.TestInvalidDeliveryInputDoesNotInvokeApplication`, `TestOutputContractStatusIsAdvisory`. |
| INT-DEL-003 public service context | `services.TestMCPExportDoesNotInferOrLeakRecords`, `environment.TestServiceContractExportResolvesFromAssets`, `TestMissingContractIsAnExplicitPreparationGap`. |
| INT-DEL-004 implementation gate | `services.TestMCPExportDoesNotInferOrLeakRecords` checks generic/unknown selection; no named third-party service is claimed. |
| INT-DEL-005 seed/response semantics | `services.TestMCPServedRecordInjectionPreservesSeedAndState`, `sandboxd.TestFileInjectionMutatesOnlyMaterializedSessionCopy`. |

Tests exercise native Go handlers and temporary source/session files without a
real model, Docker container or Operator VM. Live lifecycle/gateway and
container mount isolation remain separate integration gates.

## Dependency choice

`github.com/santhosh-tekuri/jsonschema/v6 v6.0.2` is pinned for bounded
offline JSON Schema evaluation; it is Apache-2.0 licensed. The wrapper applies
the closed profile and size/complexity checks first, disables external loading,
does not request format assertions, and does not enable type coercion or default
insertion. See the tagged [source](https://github.com/santhosh-tekuri/jsonschema/tree/v6.0.2)
and [license](https://github.com/santhosh-tekuri/jsonschema/blob/v6.0.2/LICENSE).
Dependency updates require rerunning profile rejection, body validation, fuzz
and native capability digest fixtures. The dependency's test-only regexp2
checksum does not enable regex support in Interceptor's delivery profile.
