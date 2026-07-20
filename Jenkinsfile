pipeline {
    agent {
        label '${params.AGENT_LABEL}'
    }

    options {
        buildDiscarder(logRotator(numToKeepStr: '30'))
        timestamps()
        ansiColor('xterm')
    }

    parameters {
        string(
            name: 'AGENT_LABEL',
            defaultValue: 'DATAMIGRATION',
            description: 'Jenkins agent label expression that will run this job. Use a label that does not expose secrets.'
        )
        string(
            name: 'SOURCE_DSN',
            defaultValue: 'postgres://user@source.example.com:5432/source_db?sslmode=require',
            description: 'Source PostgreSQL DSN WITHOUT the password. Password is read from DATAMIGRATION_SOURCE_DB_PASSWORD credential.'
        )
        string(
            name: 'TARGET_DSN',
            defaultValue: 'postgres://user@target.example.com:5432/target_db?sslmode=require',
            description: 'Target PostgreSQL DSN WITHOUT the password. Password is read from DATAMIGRATION_TARGET_DB_PASSWORD credential.'
        )
        string(
            name: 'SCHEMA',
            defaultValue: 'public',
            description: 'Schema name on both source and target.'
        )
        string(
            name: 'TABLE',
            defaultValue: 'metrics',
            description: 'Single table to copy (one job per table).'
        )
        string(
            name: 'BATCH_SIZE',
            defaultValue: '1000',
            description: 'Batch size for chunked copy.'
        )
        string(
            name: 'MBPS',
            defaultValue: '10',
            description: 'Max megabits per second to read from source.'
        )
        string(
            name: 'RETRIES',
            defaultValue: '5',
            description: 'Number of retries on transient errors.'
        )
        string(
            name: 'PARALLEL',
            defaultValue: '10',
            description: 'Number of parallel target write connections.'
        )
        string(
            name: 'OFFSET',
            defaultValue: '0',
            description: 'Row offset for the table copy. Not useful for hypertables.'
        )
        string(
            name: 'SKIP_UNTIL_CHUNK',
            defaultValue: '',
            description: 'Resume a hypertable copy from this chunk name. Leave empty for normal runs.'
        )
        booleanParam(
            name: 'VERBOSE',
            defaultValue: false,
            description: 'Enable verbose/debug logging.'
        )
        booleanParam(
            name: 'UPDATE',
            defaultValue: false,
            description: 'Use ON CONFLICT DO UPDATE instead of DO NOTHING.'
        )
        string(
            name: 'INSTALL_URL',
            defaultValue: 'https://raw.githubusercontent.com/warpcomdev/pgclone/main/hacks/install.sh',
            description: 'Raw GitHub URL of the install script.'
        )
        string(
            name: 'INSTALLATION_PATH',
            defaultValue: "${WORKSPACE}/.local/bin",
            description: 'Directory where pgclone will be installed for this build.'
        )
    }

    environment {
        PATH = "${params.INSTALLATION_PATH}:${env.PATH}"
    }

    stages {
        stage('Install pgclone') {
            steps {
                script {
                    echo "Installing pgclone to ${params.INSTALLATION_PATH}"
                    sh """
                        set -e
                        mkdir -p "${params.INSTALLATION_PATH}"
                        INSTALLATION_PATH="${params.INSTALLATION_PATH}" curl -fsSL "${params.INSTALL_URL}" | sh
                    """
                    sh "\"${params.INSTALLATION_PATH}/pgclone\" -help || true"
                }
            }
        }

        stage('Copy table') {
            steps {
                script {
                    // Pull passwords from Jenkins Credentials.
                    // Create credentials with IDs DATAMIGRATION_SOURCE_DB_PASSWORD and DATAMIGRATION_TARGET_DB_PASSWORD.
                    withCredentials([
                        string(credentialsId: 'DATAMIGRATION_SOURCE_DB_PASSWORD', variable: 'SOURCE_DB_PASSWORD'),
                        string(credentialsId: 'DATAMIGRATION_TARGET_DB_PASSWORD', variable: 'TARGET_DB_PASSWORD')
                    ]) {
                        // Build pgpass entries from DSNs so lib/pq can authenticate without putting
                        // passwords on the command line or in process listings.
                        // lib/pgpass format: hostname:port:database:username:password
                        def pgpassFile = "${WORKSPACE}/.pgpass"

                        writeFile file: pgpassFile, text: """${extractPgpassLine(params.SOURCE_DSN, env.SOURCE_DB_PASSWORD)}
${extractPgpassLine(params.TARGET_DSN, env.TARGET_DB_PASSWORD)}
"""
                        sh "chmod 600 ${pgpassFile}"

                        // Build optional flags
                        def verboseFlag   = params.VERBOSE ? '-verbose' : ''
                        def updateFlag    = params.UPDATE ? '-update' : ''
                        def skipChunkFlag = params.SKIP_UNTIL_CHUNK?.trim() ? "-skip-until-chunk ${shellEscape(params.SKIP_UNTIL_CHUNK.trim())}" : ''
                        def offsetFlag    = params.OFFSET?.trim() && params.OFFSET.trim() != '0' ? "-offset ${params.OFFSET.trim()}" : ''
                        def tableArg      = shellEscape(params.TABLE.trim())

                        echo "Starting pgclone for schema [${params.SCHEMA}] and table [${params.TABLE}]"

                        // Disable shell command echo to avoid printing the constructed command in logs.
                        sh """
                            set +x
                            export PGPASSFILE='${pgpassFile}'
                            '${params.INSTALLATION_PATH}/pgclone' \\
                                -source '${params.SOURCE_DSN}' \\
                                -target '${params.TARGET_DSN}' \\
                                -schema '${params.SCHEMA}' \\
                                -batch-size ${params.BATCH_SIZE} \\
                                -retries ${params.RETRIES} \\
                                -mbps ${params.MBPS} \\
                                -parallel ${params.PARALLEL} \\
                                ${offsetFlag} \\
                                ${skipChunkFlag} \\
                                ${verboseFlag} \\
                                ${updateFlag} \\
                                ${tableArg}
                        """
                    }
                }
            }
        }
    }

    post {
        always {
            script {
                // Best-effort cleanup of the pgpass file
                sh "rm -f ${WORKSPACE}/.pgpass || true"
            }
        }
        failure {
            echo 'pgclone run failed. Check the build log and the source/target database health.'
        }
        success {
            echo 'pgclone run completed successfully.'
        }
    }
}

// ---------------------------------------------------------------------------
// Helper functions
// ---------------------------------------------------------------------------

@NonCPS
String extractPgpassLine(String dsn, String password) {
    // Minimal parser for postgres://user@host:port/db?...
    // lib/pgpass format: hostname:port:database:username:password
    def m = dsn =~ /^postgres(?:ql)?:\/\/([^:@]+)(?::[^@]+)?@([^:\/]+)(?::(\d+))?\/([^?]+)/
    if (!m) {
        error "Could not parse DSN for pgpass entry: ${dsn}"
    }
    def user = m[0][1]
    def host = m[0][2]
    def port = m[0][3] ?: '5432'
    def database = m[0][4]
    return "${host}:${port}:${database}:${user}:${password}"
}

String shellEscape(String arg) {
    // Escape single quotes for safe shell inclusion
    return arg.replace("'", '"'"'"'"')
}
