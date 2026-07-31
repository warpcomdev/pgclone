pipeline {
    agent {
        label "${params.AGENT_LABEL ?: 'DATAMIGRATION'}"
    }

    options {
        buildDiscarder(logRotator(numToKeepStr: '200'))
        timestamps()
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
            defaultValue: 'https://github.com/warpcomdev/pgclone/releases/latest/download/install.sh',
            description: 'GitHub release URL of the generated install script.'
        )
        string(
            name: 'INSTALLATION_PATH',
            defaultValue: '.local/bin',
            description: 'Relative directory under WORKSPACE where pgclone will be installed for this build.'
        )
    }

    stages {
        stage('Set custom build display name') {
            steps {
                script {
                    currentBuild.displayName = "#${currentBuild.number} ${params.SCHEMA}.${params.TABLE}"
                    currentBuild.description = "Copy table ${params.SCHEMA}.${params.TABLE} from source to target."
                }
            }
        }
        stage('Install pgclone') {
            steps {
                script {
                    def buildDir = "${WORKSPACE}/.pgclone-${env.BUILD_NUMBER}"
                    def installDir = "${buildDir}/${params.INSTALLATION_PATH}"
                    def installer = "${installDir}/install.sh"
                    echo "Installing pgclone to ${installDir}"
                    sh """
                        set -e
                        mkdir -p "${installDir}"
                        curl -fsSL "${params.INSTALL_URL}" -o "${installer}"
                        INSTALLATION_PATH="${installDir}" sh "${installer}"
                        rm -f "${installer}"
                    """
                    sh "\"${installDir}/pgclone\" -help || true"
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
                        def buildDir = "${WORKSPACE}/.pgclone-${env.BUILD_NUMBER}"
                        def installDir = "${buildDir}/${params.INSTALLATION_PATH}"
                        def pgcloneLog = "${buildDir}/pgclone.log"
                        def pgcloneError = "${buildDir}/pgclone-error.txt"

                        // Build pgpass entries from DSNs so lib/pq can authenticate without putting
                        // passwords on the command line or in process listings.
                        // lib/pgpass format: hostname:port:database:username:password
                        def pgpassFile = "${buildDir}/.pgpass"
                        withEnv([
                            "PGPASS_FILE=${pgpassFile}",
                            "SOURCE_PGPASS_PREFIX=${extractPgpassPrefix(params.SOURCE_DSN)}",
                            "TARGET_PGPASS_PREFIX=${extractPgpassPrefix(params.TARGET_DSN)}"
                        ]) {
                            sh '''
                                set +x
                                umask 077
                                escape_pgpass() {
                                    # Backslashes are doubled once for Groovy and once for sed.
                                    printf '%s' "$1" | sed 's|\\\\|\\\\\\\\|g; s|:|\\\\:|g'
                                }
                                {
                                    printf '%s' "$SOURCE_PGPASS_PREFIX"
                                    escape_pgpass "$SOURCE_DB_PASSWORD"
                                    printf '\n%s' "$TARGET_PGPASS_PREFIX"
                                    escape_pgpass "$TARGET_DB_PASSWORD"
                                    printf '\n'
                                } > "$PGPASS_FILE"
                            '''
                        }

                        // Build optional flags
                        def verboseFlag   = params.VERBOSE ? '-verbose' : ''
                        def updateFlag    = params.UPDATE ? '-update' : ''
                        def skipChunkFlag = params.SKIP_UNTIL_CHUNK?.trim() ? "-skip-until-chunk ${shellEscape(params.SKIP_UNTIL_CHUNK.trim())}" : ''
                        def offsetFlag    = params.OFFSET?.trim() && params.OFFSET.trim() != '0' ? "-offset ${params.OFFSET.trim()}" : ''
                        def tableArg      = shellEscape(params.TABLE.trim())

                        echo "Starting pgclone for schema [${params.SCHEMA}] and table [${params.TABLE}]"

                        // Keep the full output as an artifact while preserving the process exit status.
                        def pgcloneStatus = sh returnStatus: true, script: """
                                set +x
                                set +e
                                export PGPASSFILE='${pgpassFile}'
                                '${installDir}/pgclone' \\
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
                                    ${tableArg} > '${pgcloneLog}' 2>&1
                                status=\$?
                                cat '${pgcloneLog}'
                                if [ "\$status" -ne 0 ]; then
                                    grep -E 'Error copying table|panic:|fatal error:|runtime error:' '${pgcloneLog}' \\
                                        | tail -n 1 > '${pgcloneError}' || true
                                    if [ ! -s '${pgcloneError}' ]; then
                                        tail -n 20 '${pgcloneLog}' > '${pgcloneError}'
                                    fi
                                fi
                                exit "\$status"
                        """

                        if (pgcloneStatus != 0) {
                            def errorSummary = fileExists(pgcloneError)
                                ? readFile(pgcloneError).trim().replaceAll(/[\r\n]+/, ' ')
                                : "pgclone exited with status ${pgcloneStatus}"
                            if (errorSummary.length() > 500) {
                                errorSummary = errorSummary.take(497) + '...'
                            }
                            currentBuild.description = "${params.SCHEMA}.${params.TABLE} failed: ${errorSummary}"
                            error("pgclone failed with exit status ${pgcloneStatus}: ${errorSummary}")
                        }
                    }
                }
            }
        }
    }

    post {
        always {
            script {
                try {
                    archiveArtifacts(
                        artifacts: ".pgclone-${env.BUILD_NUMBER}/pgclone.log,.pgclone-${env.BUILD_NUMBER}/pgclone-error.txt",
                        allowEmptyArchive: true
                    )
                } finally {
                    // Best-effort cleanup of files that may contain connection details.
                    sh "rm -rf '${WORKSPACE}/.pgclone-${env.BUILD_NUMBER}' || true"
                }
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
String extractPgpassPrefix(String dsn) {
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
    return "${host}:${port}:${database}:${user}:"
}

String shellEscape(String arg) {
    // Escape single quotes for safe shell inclusion
    return arg.replace("'", '"')
}
