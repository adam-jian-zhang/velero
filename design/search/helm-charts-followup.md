# Helm charts companion note (velero-helm-charts repo)
#
# When enabling Search with the SQLite backend, expose opt-in values analogous to
# pkg/install.VeleroOptions.SearchPVCName / SearchPVCSize:
#
#   configuration:
#     features: EnableSearch
#   search:
#     persistence:
#       enabled: true
#       existingClaim: ""          # or create a new PVC
#       size: 10Gi
#       mountPath: /var/lib/velero
#     rest:
#       enabled: false             # EnableSearchRESTAPI
#       port: 8086
#     grpc:
#       enabled: false             # EnableSearchGRPCAPI
#       port: 8087
#
# Bind ClusterRole velero-search-user to tenant ServiceAccounts that may create
# SearchRequest CRs / call the REST/gRPC APIs.
